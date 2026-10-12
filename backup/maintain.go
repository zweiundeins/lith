package backup

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/benbjohnson/litestream"
	"github.com/superfly/ltx"
)

// A bare litestream.DB never snapshots, compacts or prunes, so without these
// passes a replica only grows, and so does the time to restore it.

type maintResult struct {
	lastSnapshotAt time.Time
	objects        int
	bytes          int64
}

// snapMeta is a snapshot's time and the position it covers up to.
type snapMeta struct {
	createdAt time.Time
	maxTXID   ltx.TXID
}

// MaintainAll runs one maintenance pass on every attached replica, as of now:
// a full snapshot when the newest is older than the snapshot interval, then
// pruning of the history older than the retention window.
func (m *Manager) MaintainAll(ctx context.Context, now time.Time) map[string]error {
	m.mu.Lock()
	items := make(map[string]*attachedDB, len(m.attached))
	for name, a := range m.attached {
		items[name] = a
	}
	m.mu.Unlock()

	out := make(map[string]error, len(items))
	for name, a := range items {
		a.maint.Lock()
		if !a.detached {
			out[name] = m.maintain(ctx, name, a.db, now)
		}
		a.maint.Unlock()
	}
	return out
}

// RunMaintenanceLoop runs MaintainAll every interval
// (DefaultMaintenanceInterval if zero) until ctx is done. The interval only
// has to be shorter than the snapshot interval: a pass that is not due for a
// snapshot costs one listing per replica.
func (m *Manager) RunMaintenanceLoop(ctx context.Context, interval time.Duration) {
	if !m.enabled {
		return
	}
	t := time.NewTicker(orDefault(interval, DefaultMaintenanceInterval))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for name, err := range m.MaintainAll(ctx, now) {
				if err != nil && ctx.Err() == nil {
					m.log.Error("backup: maintenance failed", "replica", name, "error", err)
				}
			}
		}
	}
}

// maintain snapshots and prunes one replica.
//
// A restore as of T replays from the newest snapshot at or before T, so the
// oldest snapshot to keep is the newest one at or before now minus the
// window, the floor. Older snapshots, and WAL segments below the floor, serve
// no restore inside the window. While no snapshot is that old there is no
// floor and nothing is pruned, so turning this on for a replica with history
// lets that history age out instead of collapsing it.
func (m *Manager) maintain(ctx context.Context, name string, db *litestream.DB, now time.Time) error {
	client := db.Replica.Client
	snaps, err := listSnapshots(ctx, client)
	if err != nil {
		return fmt.Errorf("backup: maintain %s: %w", name, err)
	}
	if len(snaps) == 0 || now.Sub(snaps[len(snaps)-1].createdAt) >= m.snapshotInterval {
		info, err := db.Snapshot(ctx)
		if err != nil {
			return fmt.Errorf("backup: snapshot %s: %w", name, err)
		}
		if info != nil {
			snaps = append(snaps, snapMeta{createdAt: info.CreatedAt, maxTXID: info.MaxTXID})
		}
	}
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
	m.maintenance[name] = maintResult{lastSnapshotAt: last, objects: objects, bytes: bytes}
	m.mu.Unlock()
	return nil
}

// listSnapshots lists a replica's snapshots, oldest first.
func listSnapshots(ctx context.Context, client litestream.ReplicaClient) ([]snapMeta, error) {
	itr, err := client.LTXFiles(ctx, litestream.SnapshotLevel, 0, false)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	var out []snapMeta
	for itr.Next() {
		info := itr.Item()
		out = append(out, snapMeta{createdAt: info.CreatedAt, maxTXID: info.MaxTXID})
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
