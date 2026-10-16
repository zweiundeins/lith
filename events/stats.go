package events

import (
	"sync"
	"sync/atomic"

	"github.com/zweiundeins/lith/writer"
)

// StoreStats are a store's counters since the process started, over
// every database the store serves.
type StoreStats struct {
	// Appended counts the events committed, by stream kind. The kinds of
	// the store's projectors and upcasters are there from the start.
	Appended map[string]int64
	// Conflicts counts the appends that ErrConflict refused.
	Conflicts int64
	// Refolds counts the streams whose state Execute folded again from
	// their events, as their state row was missing or folded by another
	// Version of the decider.
	Refolds int64
}

// counts are a store's counters.
type counts struct {
	appended  sync.Map // stream kind: *atomic.Int64
	conflicts atomic.Int64
	refolds   atomic.Int64
}

func (c *counts) kind(kind string) *atomic.Int64 {
	if n, ok := c.appended.Load(kind); ok {
		return n.(*atomic.Int64)
	}
	n, _ := c.appended.LoadOrStore(kind, new(atomic.Int64))
	return n.(*atomic.Int64)
}

// Stats returns the store's counters.
func (st *Store) Stats() StoreStats {
	s := StoreStats{Appended: map[string]int64{}, Conflicts: st.counts.conflicts.Load(), Refolds: st.counts.refolds.Load()}
	for kind := range st.streams {
		s.Appended[kind] = 0
	}
	st.counts.appended.Range(func(kind, n any) bool {
		s.Appended[kind.(string)] = n.(*atomic.Int64).Load()
		return true
	})
	return s
}

// committed counts n events of a stream kind once the command commits,
// and moves the runner's heads to last, the newest one's position.
func (st *Store) committed(tx *writer.Tx, l *local, kind string, n int, last int64) {
	c, h := st.counts.kind(kind), l.heads
	tx.AfterCommit(func() {
		c.Add(int64(n))
		h.move(kind, last)
	})
}

// heads are the last positions committed in a runner's database: of any
// stream kind, and of each kind of the store's projectors. Only the
// writer's goroutine moves them, in the order of the commits.
type heads struct {
	all   atomic.Int64
	kinds map[string]*atomic.Int64 // fixed by newHeads
}

func newHeads(st *Store) *heads {
	h := &heads{kinds: map[string]*atomic.Int64{}}
	for _, p := range st.projectors {
		h.kinds[p.kind] = new(atomic.Int64)
	}
	return h
}

// read sets the heads from the database, in the runner's first command.
// sqlite_sequence keeps the last position handed out, also when
// retention deleted the newest events.
func (h *heads) read(tx *writer.Tx) error {
	var all int64
	if err := tx.QueryRow(`SELECT coalesce((SELECT seq FROM sqlite_sequence WHERE name = 'lith_events'), 0)`).Scan(&all); err != nil {
		return err
	}
	h.all.Store(all)
	for kind, n := range h.kinds {
		var last int64
		if err := tx.QueryRow(`SELECT coalesce(max(position), 0) FROM lith_events WHERE stream = ?`, kind).Scan(&last); err != nil {
			return err
		}
		n.Store(last)
	}
	return nil
}

func (h *heads) move(kind string, last int64) {
	if h == nil {
		return
	}
	h.all.Store(last)
	if n := h.kinds[kind]; n != nil {
		n.Store(last)
	}
}

// lag is how far pos is behind the last event of a kind, in positions.
func (h *heads) lag(kind string, pos int64) int64 {
	if n := h.kinds[kind]; n != nil {
		return max(0, n.Load()-pos)
	}
	return 0
}
