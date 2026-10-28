package backup

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/superfly/ltx"
)

// Litestream writes every WAL segment (an L0 file) to the directory beside
// the database before the replica uploads it, and deletes it only in its
// Store's L0 retention, once the segment is compacted into L1. The Manager
// runs no Store, since lith's replicas keep L0 and snapshots and nothing in
// between, so without the trim below the directory grows with every write.
//
// The trim removes a segment once the replica holds it (its TXID is at or
// below the replica's position, which only moves past a segment after its
// upload) and it is older than Config.LocalL0Retention. It never removes the
// newest segment, the one Litestream reads for its position and its next
// sync, and it removes a run of the oldest only, so the directory keeps no
// gap. Litestream's other readers of the directory are its compaction,
// which the Manager does not run, and a restart that finds the database
// behind the replica, which replaces the directory with the replica's newest
// segment.

// localL0 is the state of an attached database's local segments.
type localL0 struct {
	done chan struct{} // closed when the trim loop stops

	mu      sync.Mutex
	files   int
	bytes   int64
	removed int
}

func (l *localL0) stats() (files int, bytes int64, removed int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.files, l.bytes, l.removed
}

// trimLocalL0Loop trims at once, which clears a backlog left by an older
// version, then every m.localL0Interval until detach.
func (m *Manager) trimLocalL0Loop(name string, a *attachedDB) {
	defer close(a.local.done)
	t := time.NewTicker(m.localL0Interval)
	defer t.Stop()
	failing := 0
	for first := true; ; first = false {
		removed, bytes, err := m.trimLocalL0(a, time.Now())
		switch {
		case err != nil:
			failing++
			m.logLevel(failing)("backup: removing uploaded WAL segments from the local directory failed",
				"replica", name, "error", err, "passes", failing)
		case removed > 0 && first:
			failing = 0
			m.log.Info("backup: removed uploaded WAL segments from the local directory",
				"replica", name, "files", removed, "bytes", bytes)
		default:
			failing = 0
			if removed > 0 {
				m.log.Debug("backup: removed uploaded WAL segments from the local directory",
					"replica", name, "files", removed, "bytes", bytes)
			}
		}
		select {
		case <-a.work.Done():
			return
		case <-t.C:
		}
	}
}

// trimLocalL0 removes the local segments the replica holds that are older
// than the retention, and records what is left.
func (m *Manager) trimLocalL0(a *attachedDB, now time.Time) (removed int, removedBytes int64, err error) {
	uploaded := a.db.Replica.Pos().TXID
	dir := a.db.LTXLevelDir(0)
	ents, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, 0, fmt.Errorf("list %s: %w", dir, err)
	}
	type segment struct {
		name    string
		maxTXID ltx.TXID
		size    int64
		mod     time.Time
	}
	var segs []segment
	for _, e := range ents {
		_, maxTXID, perr := ltx.ParseFilename(e.Name())
		if perr != nil || !e.Type().IsRegular() {
			continue // a temporary file of a segment being written
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue // removed meanwhile
		}
		segs = append(segs, segment{e.Name(), maxTXID, info.Size(), info.ModTime()})
	}
	slices.SortFunc(segs, func(x, y segment) int { return cmp.Compare(x.maxTXID, y.maxTXID) })
	cutoff := now.Add(-m.localL0Retention)
	files, bytes := 0, int64(0)
	trimming := true
	for i, s := range segs {
		if trimming && i < len(segs)-1 && s.maxTXID <= uploaded && !s.mod.After(cutoff) {
			rerr := os.Remove(filepath.Join(dir, s.name))
			if rerr == nil || errors.Is(rerr, fs.ErrNotExist) {
				removed++
				removedBytes += s.size
				continue
			}
			err = fmt.Errorf("remove %s: %w", s.name, rerr)
		}
		trimming = false
		files++
		bytes += s.size
	}
	a.local.mu.Lock()
	a.local.files, a.local.bytes = files, bytes
	a.local.removed += removed
	a.local.mu.Unlock()
	return removed, removedBytes, err
}
