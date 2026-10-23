package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// Maintenance defaults. A pass over the attached replicas maintains two at
// a time and waits after each. Names not attached get a pass a day, one at a
// time with a longer wait, since each costs a client of its own (see
// MaintenanceOptions.Names). Both spread a long list, so that the storage
// provider sees no burst of listings and deletes.
const (
	DefaultMaintenanceConcurrency = 2
	DefaultMaintenancePause       = 100 * time.Millisecond
	DefaultDetachedInterval       = 24 * time.Hour
	DefaultDetachedConcurrency    = 1
	DefaultDetachedPause          = 250 * time.Millisecond
)

// MaintenanceOptions configure RunMaintenanceLoop.
type MaintenanceOptions struct {
	// Interval is the time between passes over the attached replicas; zero
	// means DefaultMaintenanceInterval. It only has to be shorter than the
	// snapshot interval.
	Interval time.Duration

	// Concurrency is the number of attached replicas maintained at once; zero
	// means DefaultMaintenanceConcurrency.
	Concurrency int

	// Pause is the wait after each attached replica before the same worker
	// takes the next; zero means DefaultMaintenancePause, a negative value
	// none. It sets a floor of 50 ms per replica with the defaults; the round
	// trips to the provider usually take longer.
	Pause time.Duration

	// Names, if set, lists the replicas of a second loop, for the ones not
	// attached: an app that attaches its tenants on demand passes its
	// account list, since a tenant not attached at the time is otherwise
	// never pruned. That loop skips the attached names. A name not attached
	// gets no snapshot, since nothing changed while it was not attached,
	// only the pruning of its history older than the retention window,
	// through the bucket alone: one listing, and one more with a snapshot
	// older than the window. A name with nothing replicated is no failure.
	//
	// Each such name takes a replica client of its own, and with S3 a new
	// HTTP transport and TLS handshake, whose idle connection lingers for
	// 90 s. There is no cache of clients, since thousands of tenants would
	// keep thousands of transports; the cost is held down by the cadence
	// instead: a pass a day, one name at a time.
	Names func(ctx context.Context) ([]string, error)

	// DetachedInterval is the time from the start of one pass over Names to
	// the start of the next; zero means DefaultDetachedInterval. The first
	// starts after at most ten minutes, and a pass that runs longer is
	// followed at once by the next, never overlapped.
	DetachedInterval time.Duration

	// DetachedConcurrency and DetachedPause are Concurrency and Pause for
	// the names not attached; zero means DefaultDetachedConcurrency and
	// DefaultDetachedPause. With the defaults 1,000 names take at least four
	// minutes, and in practice more: the round trips, a handshake and one or
	// two listings per name, dominate.
	DetachedConcurrency int
	DetachedPause       time.Duration
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
	passes   int
	failures int
	pruned   int
	lastPass time.Time // the end of the last pass whose Names succeeded
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

// deleteCounterV3 is a deleteCounter over a client that also reads v0.3
// backups, which a restore looks for with a type assertion.
type deleteCounterV3 struct {
	*deleteCounter
	v3 litestream.ReplicaClientV3
}

func (c deleteCounterV3) GenerationsV3(ctx context.Context) ([]string, error) {
	return c.v3.GenerationsV3(ctx)
}

func (c deleteCounterV3) SnapshotsV3(ctx context.Context, generation string) ([]litestream.SnapshotInfoV3, error) {
	return c.v3.SnapshotsV3(ctx, generation)
}

func (c deleteCounterV3) WALSegmentsV3(ctx context.Context, generation string) ([]litestream.WALSegmentInfoV3, error) {
	return c.v3.WALSegmentsV3(ctx, generation)
}

func (c deleteCounterV3) OpenSnapshotV3(ctx context.Context, generation string, index int) (io.ReadCloser, error) {
	return c.v3.OpenSnapshotV3(ctx, generation, index)
}

func (c deleteCounterV3) OpenWALSegmentV3(ctx context.Context, generation string, index int, offset int64) (io.ReadCloser, error) {
	return c.v3.OpenWALSegmentV3(ctx, generation, index, offset)
}

// countDeletes wraps client in a deleteCounter that keeps the optional
// interfaces litestream asserts on a client.
func countDeletes(client litestream.ReplicaClient) (litestream.ReplicaClient, *deleteCounter) {
	c := &deleteCounter{ReplicaClient: client}
	if v3, ok := client.(litestream.ReplicaClientV3); ok {
		return deleteCounterV3{c, v3}, c
	}
	return c, c
}

// MaintainAll runs one maintenance pass on every attached replica, as of now:
// a full snapshot when the newest is older than the snapshot interval, then
// pruning of the history older than the retention window.
func (m *Manager) MaintainAll(ctx context.Context, now time.Time) map[string]error {
	return m.maintainPass(ctx, m.attachedNames(), 1, 0, func(name string) (bool, error) {
		return m.maintainAttached(ctx, name, now)
	})
}

// RunMaintenanceLoop maintains the attached replicas every opt.Interval and,
// when opt.Names is set, the names not attached every opt.DetachedInterval,
// until ctx is done. An attached replica costs a listing of each level per
// pass, for Status. Its failure is logged as a warning, since the next pass
// retries it, and as an error from its third failed pass in a row on. The
// names not attached that fail are logged once per pass, as a count with the
// first few names, by the same rule, and so is a failure of opt.Names.
func (m *Manager) RunMaintenanceLoop(ctx context.Context, opt MaintenanceOptions) {
	if !m.enabled {
		return
	}
	var wg sync.WaitGroup
	if opt.Names != nil {
		wg.Go(func() { m.runDetachedLoop(ctx, opt) })
	}
	defer wg.Wait()

	workers, pause := spread(opt.Concurrency, opt.Pause, DefaultMaintenanceConcurrency, DefaultMaintenancePause)
	var failing map[string]int
	runEvery(ctx, orDefault(opt.Interval, DefaultMaintenanceInterval), func(now time.Time) {
		results := m.maintainPass(ctx, m.attachedNames(), workers, pause, func(name string) (bool, error) {
			return m.maintainAttached(ctx, name, now)
		})
		if ctx.Err() == nil {
			failing = m.logMaintenance(results, failing)
		}
	})
}

// runEvery calls pass every interval until ctx is done, never two at once.
func runEvery(ctx context.Context, interval time.Duration, pass func(now time.Time)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			pass(now)
		}
	}
}

func spread(workers int, pause time.Duration, defWorkers int, defPause time.Duration) (int, time.Duration) {
	if workers <= 0 {
		workers = defWorkers
	}
	if pause == 0 {
		pause = defPause
	}
	return workers, pause
}

// runDetachedLoop runs a pass over opt.Names like the verify loop: the
// first after at most ten minutes, then one every interval from the start
// of the one before.
func (m *Manager) runDetachedLoop(ctx context.Context, opt MaintenanceOptions) {
	interval := orDefault(opt.DetachedInterval, DefaultDetachedInterval)
	timer := time.NewTimer(min(10*time.Minute, interval))
	defer timer.Stop()
	l := detachedLoop{rejected: map[string]bool{}}
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		start := time.Now()
		l.pass(ctx, m, start, opt)
		timer.Reset(time.Until(start.Add(interval)))
	}
}

// detachedLoop is what the loop over the names not attached keeps from pass
// to pass: its failed passes in a row, of Names and of any replica, and the
// names checkName rejected, each logged once.
type detachedLoop struct {
	namesFailing int
	failing      int
	rejected     map[string]bool
}

// maxNamesLogged is the number of failed names a pass's summary lists.
const maxNamesLogged = 5

func (l *detachedLoop) pass(ctx context.Context, m *Manager, now time.Time, opt MaintenanceOptions) {
	list, err := opt.Names(ctx)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		l.namesFailing++
		m.logLevel(l.namesFailing)("backup: listing the replicas to maintain failed; the replicas not attached wait for the next pass",
			"error", err, "passes", l.namesFailing)
		return
	}
	l.namesFailing = 0
	var names []string
	for _, name := range list {
		if err := checkName(name); err != nil {
			if !l.rejected[name] {
				l.rejected[name] = true
				m.log.Warn("backup: maintenance skips a name it cannot replicate", "error", err)
			}
			continue
		}
		names = append(names, name)
	}
	workers, pause := spread(opt.DetachedConcurrency, opt.DetachedPause, DefaultDetachedConcurrency, DefaultDetachedPause)
	results := m.maintainNames(ctx, now, names, workers, pause)
	if ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	m.detachedMaint.lastPass = time.Now()
	m.mu.Unlock()

	var failed []string
	for _, name := range slices.Sorted(maps.Keys(results)) {
		if results[name] != nil {
			failed = append(failed, name)
		}
	}
	if len(failed) == 0 {
		l.failing = 0
		return
	}
	l.failing++
	m.logLevel(l.failing)("backup: maintenance of replicas not attached failed",
		"failed", len(failed), "of", len(results), "replicas", failed[:min(len(failed), maxNamesLogged)],
		"error", results[failed[0]], "passes", l.failing)
}

// logLevel is a warning until the third failure in a row, then an error.
func (m *Manager) logLevel(inARow int) func(msg string, args ...any) {
	if inARow < maintenanceErrorAfter {
		return m.log.Warn
	}
	return m.log.Error
}

// maintainNames prunes names that are not attached, each once.
func (m *Manager) maintainNames(ctx context.Context, now time.Time, names []string, workers int, pause time.Duration) map[string]error {
	mark := m.purgeMark()
	list := slices.Compact(slices.Sorted(slices.Values(names)))
	return m.maintainPass(ctx, list, workers, pause, func(name string) (bool, error) {
		return m.maintainNamed(ctx, name, now, mark)
	})
}

// maintainPass runs maintain on each of list, up to workers at a time, with
// a pause after each it did not skip, and returns the result of each.
func (m *Manager) maintainPass(ctx context.Context, list []string, workers int, pause time.Duration, maintain func(name string) (ok bool, err error)) map[string]error {
	jobs := make(chan string)
	var mu sync.Mutex
	out := map[string]error{}
	var wg sync.WaitGroup
	for range min(workers, len(list)) {
		wg.Go(func() {
			for name := range jobs {
				if ctx.Err() != nil {
					continue
				}
				ok, err := maintain(name)
				if !ok {
					continue
				}
				mu.Lock()
				out[name] = err
				mu.Unlock()
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

// maintainAttached maintains an attached replica; ok is false when it was
// skipped: detached meanwhile, or pruned by the loop over the names not
// attached right now.
func (m *Manager) maintainAttached(ctx context.Context, name string, now time.Time) (ok bool, err error) {
	m.mu.Lock()
	a, busy := m.attached[name], m.pruning[name]
	m.mu.Unlock()
	if a == nil || busy {
		return false, nil
	}
	a.maint.Lock()
	defer a.maint.Unlock()
	if a.detached {
		return false, nil
	}
	return true, m.maintain(ctx, name, a, now)
}

// maintainNamed prunes a replica that is not attached; ok is false when it
// was skipped: attached, being attached or detached, or purged since mark.
func (m *Manager) maintainNamed(ctx context.Context, name string, now time.Time, mark uint64) (ok bool, err error) {
	if checkName(name) != nil {
		return false, nil
	}
	m.mu.Lock()
	if m.attached[name] != nil || m.reserved[name] || m.purges[name] > mark {
		m.mu.Unlock()
		return false, nil
	}
	m.pruning[name] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.pruning, name)
		m.mu.Unlock()
	}()

	// An Attach that starts after the check above overlaps this prune
	// safely: its first sync writes WAL segments above every position here,
	// the prune deletes nothing at or above the floor, nor the newest
	// segment, and the attached pass skips the name until the prune is done.
	pruned, err := m.maintainDetached(ctx, name, now)
	if ctx.Err() != nil {
		return false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.purges[name] > mark {
		return false, nil
	}
	m.detachedMaint.pruned += pruned
	if err != nil {
		m.detachedMaint.failures++
	} else {
		m.detachedMaint.passes++
	}
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
	snaps, err := m.snapshotIfDue(ctx, name, db, now)
	if err != nil {
		return err
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

// snapshotIfDue writes a full snapshot of db when the newest is older than
// the snapshot interval at now, and returns the snapshots, oldest first.
func (m *Manager) snapshotIfDue(ctx context.Context, name string, db *litestream.DB, now time.Time) ([]snapMeta, error) {
	snaps, err := listSnapshots(ctx, db.Replica.Client)
	if err != nil {
		return nil, fmt.Errorf("backup: maintain %s: %w", name, err)
	}
	if len(snaps) > 0 && now.Sub(snaps[len(snaps)-1].createdAt) < m.snapshotInterval {
		return snaps, nil
	}
	pos, err := db.Pos()
	if err != nil {
		return nil, fmt.Errorf("backup: maintain %s: %w", name, err)
	}
	// A snapshot is named by its position, so one at a position already
	// snapshotted would replace that snapshot and move its time forward.
	if slices.ContainsFunc(snaps, func(s snapMeta) bool { return s.maxTXID == pos.TXID }) {
		return snaps, nil
	}
	info, err := db.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup: snapshot %s: %w", name, err)
	}
	if info != nil {
		snaps = slices.DeleteFunc(snaps, func(s snapMeta) bool { return s.maxTXID == info.MaxTXID })
		snaps = append(snaps, snapMeta{createdAt: info.CreatedAt, maxTXID: info.MaxTXID, info: info})
	}
	return snaps, nil
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
