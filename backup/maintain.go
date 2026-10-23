package backup

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/benbjohnson/litestream"
	"github.com/superfly/ltx"
)

// A bare litestream.DB never snapshots, compacts or prunes, so without these
// passes a replica only grows, and so does the time to restore it.

// Maintenance defaults: a pass maintains two replicas at a time and waits
// after each, so that a long list of names does not burst listings and
// deletes at the storage provider.
const (
	DefaultMaintenanceConcurrency = 2
	DefaultMaintenancePause       = 100 * time.Millisecond
)

// MaintenanceOptions configure RunMaintenanceLoop.
type MaintenanceOptions struct {
	// Interval is the time between passes; zero means
	// DefaultMaintenanceInterval. It only has to be shorter than the snapshot
	// interval.
	Interval time.Duration

	// Names, if set, lists replicas each pass maintains besides the attached
	// ones: an app that attaches its tenants on demand passes its account
	// list, since a tenant not attached at the time is otherwise never
	// pruned. A name that is attached is maintained as an attached replica.
	// One that is not gets no snapshot, since nothing changed while it was
	// detached, only the pruning of its history older than the retention
	// window, through the bucket alone. A name with nothing replicated under
	// it costs one listing and is no failure. When Names fails, the pass
	// maintains the attached replicas only.
	Names func(ctx context.Context) ([]string, error)

	// Concurrency is the number of replicas maintained at once; zero means
	// DefaultMaintenanceConcurrency.
	Concurrency int

	// Pause is the wait after each replica before the same worker takes the
	// next, so that a long list spreads its requests over the pass; zero
	// means DefaultMaintenancePause, a negative value none. With the
	// defaults a pass takes at least 50 ms per replica: 1,000 names take a
	// minute.
	Pause time.Duration
}

type maintResult struct {
	lastSnapshotAt time.Time
	objects        int
	bytes          int64

	// Passes while attached; detached ones count in Manager.detachedMaint.
	passes int
	pruned int
}

// maintTotals sums the passes over replicas that were not attached.
type maintTotals struct {
	passes int
	pruned int
}

// snapMeta is a snapshot's time and the position it covers up to.
type snapMeta struct {
	createdAt time.Time
	maxTXID   ltx.TXID
	info      *ltx.FileInfo
}

// deleteCounter counts the LTX files its client deleted.
type deleteCounter struct {
	litestream.ReplicaClient
	n atomic.Int64
}

func (c *deleteCounter) DeleteLTXFiles(ctx context.Context, a []*ltx.FileInfo) error {
	if err := c.ReplicaClient.DeleteLTXFiles(ctx, a); err != nil {
		return err
	}
	c.n.Add(int64(len(a)))
	return nil
}

// MaintainAll runs one maintenance pass on every attached replica, as of now:
// a full snapshot when the newest is older than the snapshot interval, then
// pruning of the history older than the retention window.
func (m *Manager) MaintainAll(ctx context.Context, now time.Time) map[string]error {
	return m.maintainPass(ctx, now, nil, 1, 0)
}

// RunMaintenanceLoop runs a maintenance pass every opt.Interval until ctx is
// done, over the attached replicas and opt.Names. A replica not attached
// costs one listing per pass, and one more once it has a snapshot older than
// the retention window; an attached one also lists every level for Status.
// A replica's failure is logged as a warning, since the next pass retries
// it, and as an error from its third failed pass in a row on; so is a
// failure of opt.Names.
func (m *Manager) RunMaintenanceLoop(ctx context.Context, opt MaintenanceOptions) {
	if !m.enabled {
		return
	}
	t := time.NewTicker(orDefault(opt.Interval, DefaultMaintenanceInterval))
	defer t.Stop()
	var l maintLoop
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			l.pass(ctx, m, now, opt)
		}
	}
}

// maintLoop is what the maintenance loop keeps from pass to pass: the failed
// passes in a row of each failing replica, and of Names.
type maintLoop struct {
	failing      map[string]int
	namesFailing int
}

func (l *maintLoop) pass(ctx context.Context, m *Manager, now time.Time, opt MaintenanceOptions) {
	var names []string
	if opt.Names != nil {
		list, err := opt.Names(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			l.namesFailing++
			log := m.log.Warn
			if l.namesFailing >= maintenanceErrorAfter {
				log = m.log.Error
			}
			log("backup: listing the replicas to maintain failed; this pass maintains the attached ones", "error", err, "passes", l.namesFailing)
		} else {
			l.namesFailing = 0
			names = list
		}
	}
	workers := opt.Concurrency
	if workers <= 0 {
		workers = DefaultMaintenanceConcurrency
	}
	pause := opt.Pause
	if pause == 0 {
		pause = DefaultMaintenancePause
	}
	results := m.maintainPass(ctx, now, names, workers, pause)
	if ctx.Err() == nil {
		l.failing = m.logMaintenance(results, l.failing)
	}
}

// maintainPass maintains the attached replicas and names, each once, on up
// to workers at a time. A name being attached or detached is left to the
// next pass, and one purged during the pass gets no result.
func (m *Manager) maintainPass(ctx context.Context, now time.Time, names []string, workers int, pause time.Duration) map[string]error {
	mark := m.purgeMark()
	list := slices.Compact(slices.Sorted(slices.Values(append(m.attachedNames(), names...))))

	jobs := make(chan string)
	var mu sync.Mutex
	out := map[string]error{}
	var wg sync.WaitGroup
	for range min(workers, len(list)) {
		wg.Go(func() {
			for name := range jobs {
				ok, err := m.maintainName(ctx, name, now, mark)
				if ok {
					mu.Lock()
					out[name] = err
					mu.Unlock()
				}
				if pause > 0 {
					t := time.NewTimer(pause)
					select {
					case <-ctx.Done():
					case <-t.C:
					}
					t.Stop()
				}
			}
		})
	}
	for _, name := range list {
		if ctx.Err() != nil {
			break
		}
		jobs <- name
	}
	close(jobs)
	wg.Wait()
	return out
}

// maintainName maintains one replica; ok is false when it was skipped.
func (m *Manager) maintainName(ctx context.Context, name string, now time.Time, mark uint64) (ok bool, err error) {
	if ctx.Err() != nil || checkName(name) != nil {
		return false, nil
	}
	m.mu.Lock()
	a, busy := m.attached[name], m.reserved[name]
	m.mu.Unlock()
	if a != nil {
		a.maint.Lock()
		defer a.maint.Unlock()
		if a.detached {
			return false, nil
		}
		return true, m.maintain(ctx, name, a, now)
	}
	if busy || m.purgedSince(name, mark) {
		return false, nil
	}
	pruned, err := m.maintainDetached(ctx, name, now)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.purges[name] > mark {
		return false, nil
	}
	if err == nil {
		m.detachedMaint.passes++
	}
	m.detachedMaint.pruned += pruned
	return true, err
}

// maintenanceErrorAfter is the number of failed passes in a row from which a
// replica's maintenance failure is an error.
const maintenanceErrorAfter = 3

// logMaintenance logs a pass's failures and returns each failing replica's
// count of failed passes in a row.
func (m *Manager) logMaintenance(results map[string]error, failing map[string]int) map[string]int {
	next := map[string]int{}
	for _, name := range slices.Sorted(maps.Keys(results)) {
		err := results[name]
		if err == nil {
			continue
		}
		next[name] = failing[name] + 1
		if next[name] < maintenanceErrorAfter {
			m.log.Warn("backup: maintenance failed; the next pass retries it", "replica", name, "error", err, "passes", next[name])
		} else {
			m.log.Error("backup: maintenance failed", "replica", name, "error", err, "passes", next[name])
		}
	}
	return next
}

// maintain snapshots one attached replica when due, then prunes what no
// restore inside the window needs: everything below the floor, the newest
// snapshot at or before now minus the window. Without such a snapshot
// nothing is pruned.
func (m *Manager) maintain(ctx context.Context, name string, a *attachedDB, now time.Time) error {
	db := a.db
	client := db.Replica.Client
	snaps, err := listSnapshots(ctx, client)
	if err != nil {
		return fmt.Errorf("backup: maintain %s: %w", name, err)
	}
	if len(snaps) == 0 || now.Sub(snaps[len(snaps)-1].createdAt) >= m.snapshotInterval {
		pos, err := db.Pos()
		if err != nil {
			return fmt.Errorf("backup: maintain %s: %w", name, err)
		}
		// A snapshot is named by its position, so one at a position already
		// snapshotted would replace that snapshot and move its time forward.
		if !slices.ContainsFunc(snaps, func(s snapMeta) bool { return s.maxTXID == pos.TXID }) {
			info, err := db.Snapshot(ctx)
			if err != nil {
				return fmt.Errorf("backup: snapshot %s: %w", name, err)
			}
			if info != nil {
				snaps = slices.DeleteFunc(snaps, func(s snapMeta) bool { return s.maxTXID == info.MaxTXID })
				snaps = append(snaps, snapMeta{createdAt: info.CreatedAt, maxTXID: info.MaxTXID, info: info})
			}
		}
	}
	before := a.deletes.n.Load()
	defer func() {
		m.mu.Lock()
		r := m.maintenance[name]
		r.pruned += int(a.deletes.n.Load() - before)
		m.maintenance[name] = r
		m.mu.Unlock()
	}()
	if floor, ok := newestAtOrBefore(snaps, now.Add(-m.retentionWindow)); ok {
		if _, err := db.EnforceSnapshotRetention(ctx, floor.createdAt); err != nil {
			return fmt.Errorf("backup: prune snapshots of %s: %w", name, err)
		}
		// Litestream keeps the newest file of a level even when all qualify.
		if err := db.EnforceRetentionByTXID(ctx, 0, floor.maxTXID); err != nil {
			return fmt.Errorf("backup: prune WAL segments of %s: %w", name, err)
		}
	}

	var last time.Time
	if len(snaps) > 0 {
		last = snaps[len(snaps)-1].createdAt
	}
	objects, bytes := measure(ctx, client)
	m.mu.Lock()
	r := m.maintenance[name]
	r.lastSnapshotAt, r.objects, r.bytes = last, objects, bytes
	r.passes++
	m.maintenance[name] = r
	m.mu.Unlock()
	return nil
}

// maintainDetached prunes a replica that is not attached by maintain's rule,
// through the bucket alone, and returns the number of objects it deleted.
func (m *Manager) maintainDetached(ctx context.Context, name string, now time.Time) (int, error) {
	client := &deleteCounter{ReplicaClient: m.newClient(name)}
	pruned := func() int { return int(client.n.Load()) }
	snaps, err := listSnapshots(ctx, client)
	if err != nil {
		return 0, fmt.Errorf("backup: maintain %s: %w", name, err)
	}
	floor, ok := newestAtOrBefore(snaps, now.Add(-m.retentionWindow))
	if !ok {
		return 0, nil
	}
	var old []*ltx.FileInfo
	for _, s := range snaps {
		if s.createdAt.Before(floor.createdAt) {
			old = append(old, s.info)
		}
	}
	if len(old) > 0 {
		if err := client.DeleteLTXFiles(ctx, old); err != nil {
			return pruned(), fmt.Errorf("backup: prune snapshots of %s: %w", name, err)
		}
	}
	c := litestream.NewCompactor(client, m.log.With("replica", name))
	if err := c.EnforceRetentionByTXID(ctx, 0, floor.maxTXID); err != nil {
		return pruned(), fmt.Errorf("backup: prune WAL segments of %s: %w", name, err)
	}
	return pruned(), nil
}

// listSnapshots lists a replica's snapshots, oldest first.
func listSnapshots(ctx context.Context, client litestream.ReplicaClient) ([]snapMeta, error) {
	itr, err := client.LTXFiles(ctx, litestream.SnapshotLevel, 0, false)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	var out []snapMeta
	for itr.Next() {
		info := *itr.Item()
		out = append(out, snapMeta{createdAt: info.CreatedAt, maxTXID: info.MaxTXID, info: &info})
	}
	if err := errors.Join(itr.Err(), itr.Close()); err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].createdAt.Before(out[j].createdAt) })
	return out, nil
}

// newestAtOrBefore returns the newest of snaps (oldest first) not after
// cutoff.
func newestAtOrBefore(snaps []snapMeta, cutoff time.Time) (snapMeta, bool) {
	var best snapMeta
	found := false
	for _, s := range snaps {
		if s.createdAt.After(cutoff) {
			break
		}
		best, found = s, true
	}
	return best, found
}

// measure counts a replica's objects and bytes over every level. It feeds
// only Status, so a level that fails to list is skipped.
func measure(ctx context.Context, client litestream.ReplicaClient) (objects int, bytes int64) {
	for level := 0; level <= litestream.SnapshotLevel; level++ {
		itr, err := client.LTXFiles(ctx, level, 0, false)
		if err != nil {
			continue
		}
		for itr.Next() {
			objects++
			bytes += itr.Item().Size
		}
		_ = itr.Close()
	}
	return objects, bytes
}
