package events

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"
)

// Retention says which events of a stream kind Prune and Compact keep. An
// event goes only when every rule that is set lets it go: with both, the
// events of the last 30 days stay, and the newest 1,000 also when they are
// older.
type Retention struct {
	// Since keeps the events stored at or after this time. Zero sets no
	// rule of time.
	Since time.Time
	// Newest keeps this many of the kind's newest events. 0 sets no rule
	// of count.
	Newest int
	// Limit is the most events one call takes, the oldest first (0: no
	// limit), so that a sweep spreads a long backlog over its runs.
	Limit int
}

// Forget deletes every event of a stream and its state row, as a person's
// erasure asks. Nothing can be rebuilt from them afterwards: delete what
// the projections hold of the stream in the same command. A catch-up
// projector that has not reached them never sees them. It returns how many
// events it deleted.
func Forget[K Key, E any](st *Store, tx *writer.Tx, id StreamID[K, E]) (int64, error) {
	if _, _, err := st.prepare(tx, ""); err != nil {
		return 0, err
	}
	key := keyValue(id.key)
	res, err := tx.Exec(`DELETE FROM lith_events WHERE stream = ? AND stream_id = ?`, id.s.kind, key)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM lith_event_state WHERE stream = ? AND stream_id = ?`, id.s.kind, key); err != nil {
		return 0, err
	}
	tx.Touch(Topic)
	return n, nil
}

// Prune deletes the events of stream kind s that keep lets go, oldest
// first, and returns how many it deleted: retention for a kind whose
// history need not stay, sent from a sweep. It keeps the state rows and
// the projections built from the events, so commands and pages go on as
// before; but a rebuild or a new projector sees only the events kept, so a
// stream whose events all went comes back as if it had none, and a
// decider whose Version changes keeps the state of a stream that lost its
// first events as it was. Compact keeps what they need. Prune deletes no
// event that a catch-up projector of the kind, or a rebuild under way, has
// not passed.
func Prune[K Key, E any](st *Store, tx *writer.Tx, s *Stream[K, E], keep Retention) (int64, error) {
	if _, _, err := st.prepare(tx, ""); err != nil {
		return 0, err
	}
	cut, err := st.cut(tx, s.kind, keep, 0)
	if err != nil || cut <= 1 {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM lith_events WHERE position IN
		(SELECT position FROM lith_events WHERE stream = ? AND position < ? ORDER BY position LIMIT ?)`, s.kind, cut, limit(keep))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if n > 0 {
		tx.Touch(Topic)
	}
	return n, err
}

// Compact is Prune that closes the books: each stream that loses events
// keeps one, its last event that keep lets go, rewritten in place as a
// snapshot of the stream's state as of that event. The snapshot is an
// event of type snapshot, which the app defines on the stream like any
// other; of makes it from the state that d folds, as StateAt does. It
// keeps the event's position, version, time and metadata, and only its
// type and data change. d's Evolve sets the state from a snapshot, and
// every projector of the stream handles or ignores it, as New checks: a
// rebuild or a new projector makes from it what the events it replaces
// made, and a decider whose Version changes folds from it.
//
// Rewriting a stored event is the one deliberate exception to events
// never changing: the snapshot holds what the events it replaces held, at
// the place of the last of them. Positions stay in order, so StateAt,
// LoadUntil and a projection that keeps positions read as before from
// Bounds' first position on, the newest snapshot. Each stream keeps one
// snapshot at most, so the snapshots are bounded by the number of
// streams; a later call replaces the snapshot of a stream that loses
// events again, and leaves the others alone.
//
// Compact returns how many events it deleted. Like Prune, it keeps the
// state rows and the projections, and goes no further than a catch-up
// projector of the kind, or a rebuild under way, has passed. It refuses a
// stream whose first events Prune took, as it cannot fold its state. Its
// time grows with the events it takes, not with those the streams keep,
// so a sweep with a Limit stays short on a long stream.
func Compact[K Key, E, T, C, S any](st *Store, tx *writer.Tx, snapshot Type[K, E, T], keep Retention, d Decider[C, E, S], of func(S) T) (int64, error) {
	s := snapshot.s
	if err := checkDecider[S](st, s.kind, d.Version); err != nil {
		return 0, err
	}
	if _, _, err := st.prepare(tx, ""); err != nil {
		return 0, err
	}
	if up := st.upcasts[s.kind][snapshot.name]; up != nil {
		return 0, fmt.Errorf("events: %s is read as %s: compact into that", snapshot.name, up.to)
	}
	ctx := tx.Context()
	from, err := compacted(ctx, tx, s.kind)
	if err != nil {
		return 0, err
	}
	cut, err := st.cut(tx, s.kind, keep, from)
	if err != nil || cut <= from+1 {
		return 0, err
	}
	// The events since the last call that keep lets go, and the last of
	// each stream among them.
	rows, err := tx.Query(`SELECT position, stream_id FROM lith_events
		WHERE stream = ? AND position > ? AND position < ? ORDER BY position LIMIT ?`, s.kind, from, cut, limit(keep))
	if err != nil {
		return 0, err
	}
	last := map[any]int64{}
	var through int64
	for rows.Next() {
		var key any
		if err := rows.Scan(&through, &key); err != nil {
			rows.Close()
			return 0, err
		}
		last[key] = through
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()
	if through == 0 {
		return 0, nil
	}
	type end struct {
		key      K
		position int64
	}
	ends := make([]end, 0, len(last))
	for v, pos := range last {
		key, err := keyFrom[K](v)
		if err != nil {
			return 0, err
		}
		ends = append(ends, end{key, pos})
	}
	slices.SortFunc(ends, func(a, b end) int { return cmp.Compare(a.position, b.position) })
	var deleted int64
	for _, c := range ends {
		state, version, err := StateAt(ctx, st, tx, d, s.ID(c.key), c.position)
		if err != nil {
			return 0, err
		}
		data, err := json.Marshal(of(state))
		if err != nil {
			return 0, fmt.Errorf("events: encode %s: %w", snapshot.name, err)
		}
		// version is the event's at c.position: the events before it by
		// version are those before it by position, and the index finds them.
		res, err := tx.Exec(`DELETE FROM lith_events WHERE stream = ? AND stream_id = ? AND version < ?`, s.kind, keyValue(c.key), version)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		deleted += n
		if _, err := tx.Exec(`UPDATE lith_events SET type = ?, data = ? WHERE position = ?`, snapshot.name, string(data), c.position); err != nil {
			return 0, err
		}
	}
	_, err = tx.Exec(`INSERT INTO lith_event_kinds (stream, compacted) VALUES (?, ?)
		ON CONFLICT (stream) DO UPDATE SET compacted = excluded.compacted`, s.kind, through)
	if err != nil {
		return 0, err
	}
	tx.Touch(Topic)
	return deleted, nil
}

// cut returns the position before which keep lets the events of a kind
// go, no further than a catch-up projector of the kind, or a rebuild under
// way, has passed; 0 when none go. Since looks for the first event to keep
// after position after only: Compact settled the events up to it.
func (st *Store) cut(tx *writer.Tx, kind string, keep Retention, after int64) (int64, error) {
	if keep.Since.IsZero() && keep.Newest <= 0 {
		return 0, errors.New("events: a Retention keeps the events Since a time, the Newest, or both")
	}
	cut := int64(math.MaxInt64)
	if !keep.Since.IsZero() {
		err := tx.QueryRow(`SELECT position FROM lith_events WHERE stream = ? AND position > ? AND at >= ? ORDER BY position LIMIT 1`,
			kind, after, keep.Since.UnixMilli()).Scan(&cut)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	if keep.Newest > 0 {
		var pos int64
		err := tx.QueryRow(`SELECT position FROM lith_events WHERE stream = ? ORDER BY position DESC LIMIT 1 OFFSET ?`,
			kind, keep.Newest-1).Scan(&pos)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return 0, nil // no more than Newest
		case err != nil:
			return 0, err
		}
		cut = min(cut, pos)
	}
	for _, p := range st.projectors {
		if p.kind != kind {
			continue
		}
		var pos int64
		var status string
		err := tx.QueryRow(`SELECT position, status FROM lith_projections WHERE name = ?`, p.name).Scan(&pos, &status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if p.catchUp || status == statusRebuilding {
			cut = min(cut, pos+1)
		}
	}
	return cut, nil
}

// limit is keep's Limit as SQL's LIMIT takes it.
func limit(keep Retention) int {
	if keep.Limit <= 0 {
		return -1
	}
	return keep.Limit
}

// compacted returns the position up to which Compact rewrote a stream
// kind's events as snapshots, 0 where it never did.
func compacted(ctx context.Context, q sqlite.Querier, kind string) (int64, error) {
	var pos int64
	err := q.QueryRowContext(ctx, `SELECT compacted FROM lith_event_kinds WHERE stream = ?`, kind).Scan(&pos)
	if err != nil && (errors.Is(err, sql.ErrNoRows) || noTable(err)) {
		return 0, nil
	}
	return pos, err
}
