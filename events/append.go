package events

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/zweiundeins/lith/writer"
)

// Decider is the write side of a kind of stream for commands of type C,
// in the functional decider pattern: what a command needs to decide,
// folded from the stream's own events. Decide and Evolve are pure, so a
// test runs them without a database (eventstest).
type Decider[C, E, S any] struct {
	// Initial is the state of a stream without events.
	Initial S
	// Decide returns the events that cmd causes in state s, or the error
	// that refuses it. No events: nothing changes.
	Decide func(cmd C, s S) ([]E, error)
	// Evolve returns the state after event e.
	Evolve func(s S, e E) S
	// Version is the version of Evolve and of the state's shape. The store
	// keeps each stream's state as JSON in a row, with the version it was
	// folded with; a stream whose row has another version is folded again
	// from its events at its next command. Bump it whenever Evolve or the
	// state change.
	Version int
}

// Execute runs a command on a stream, in the command's transaction: it
// reads the stream's state row (folding the stream's events when there is
// none, or one of another Version), checks the expected version, decides,
// appends the events, saves the state row and runs the inline projectors.
// An error from any of them leaves the command to fail, and its savepoint
// takes back everything. The deciders of a stream kind, one per command
// type, share the state type, Evolve and Version; Execute refuses a
// decider whose state type or Version differs from the first it saw for
// the kind.
func Execute[K Key, C, E, S any](st *Store, tx *writer.Tx, d Decider[C, E, S], id StreamID[K, E], want Expected, cmd C, meta Meta) error {
	if err := checkDecider[S](st, id.s.kind, d.Version); err != nil {
		return err
	}
	l, live, err := st.prepare(tx, id.s.kind)
	if err != nil {
		return err
	}
	key := keyValue(id.key)
	s, version, refolded, err := state(st, tx, d, id, key)
	if err != nil {
		return err
	}
	if refolded && version > 0 {
		st.counts.refolds.Add(1)
	}
	if err := want.check(id.s.kind, key, version); err != nil {
		st.counts.conflicts.Add(1)
		return err
	}
	evs, err := d.Decide(cmd, s)
	if err != nil {
		return err
	}
	if len(evs) == 0 {
		if refolded {
			return saveState(tx, id.s.kind, key, version, d.Version, s)
		}
		return nil
	}
	recs, err := appendEvents(st, tx, l, id, key, version, meta, evs)
	if err != nil {
		return err
	}
	for _, e := range evs {
		s = d.Evolve(s, e)
	}
	if err := saveState(tx, id.s.kind, key, version+int64(len(evs)), d.Version, s); err != nil {
		return err
	}
	return project(st, tx, live, id.s.kind, recs)
}

// Append appends events that need no decision, such as an import's or a
// migration's, after checking the expected version, and runs the inline
// projectors, in the command's transaction. The stream's state row, if a
// decider made one, is dropped, so the stream's next Execute refolds it
// from all its events.
func Append[K Key, E any](st *Store, tx *writer.Tx, id StreamID[K, E], want Expected, meta Meta, evs ...E) error {
	l, live, err := st.prepare(tx, id.s.kind)
	if err != nil {
		return err
	}
	key := keyValue(id.key)
	version, stateful, err := currentVersion(tx, id.s.kind, key)
	if err != nil {
		return err
	}
	if err := want.check(id.s.kind, key, version); err != nil {
		st.counts.conflicts.Add(1)
		return err
	}
	if len(evs) == 0 {
		return nil
	}
	recs, err := appendEvents(st, tx, l, id, key, version, meta, evs)
	if err != nil {
		return err
	}
	if stateful {
		if _, err := tx.Exec(`DELETE FROM lith_event_state WHERE stream = ? AND stream_id = ?`, id.s.kind, key); err != nil {
			return err
		}
	}
	return project(st, tx, live, id.s.kind, recs)
}

// currentVersion returns a stream's version, and whether it has a state
// row.
func currentVersion(tx *writer.Tx, kind string, key any) (int64, bool, error) {
	var v int64
	err := tx.QueryRow(`SELECT version FROM lith_event_state WHERE stream = ? AND stream_id = ?`, kind, key).Scan(&v)
	if err == nil {
		return v, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	err = tx.QueryRow(`SELECT coalesce(max(version), 0) FROM lith_events WHERE stream = ? AND stream_id = ?`, kind, key).Scan(&v)
	return v, false, err
}

// state returns a stream's state and version, from its row or folded from
// its events, and whether it was folded.
func state[K Key, C, E, S any](st *Store, tx *writer.Tx, d Decider[C, E, S], id StreamID[K, E], key any) (s S, version int64, refolded bool, err error) {
	var fold int
	var data string
	err = tx.QueryRow(`SELECT version, fold, state FROM lith_event_state WHERE stream = ? AND stream_id = ?`, id.s.kind, key).Scan(&version, &fold, &data)
	stored := err == nil
	switch {
	case stored && fold == d.Version:
		err = json.Unmarshal([]byte(data), &s)
		if err != nil {
			err = fmt.Errorf("events: the state of stream %s %v: %w", id.s.kind, key, err)
		}
		return s, version, false, err
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return s, 0, false, err
	}
	if s, err = initial(d); err != nil {
		return s, 0, false, err
	}
	rows, err := tx.Query(`SELECT position, version, type, data, at, actor, correlation, causation
		FROM lith_events WHERE stream = ? AND stream_id = ? ORDER BY version`, id.s.kind, key)
	if err != nil {
		return s, 0, false, err
	}
	defer rows.Close()
	var last int64
	for rows.Next() {
		ev, err := scanRaw(rows, id.s.kind, key)
		if err != nil {
			return s, 0, false, err
		}
		if last == 0 && ev.Version != 1 {
			if stored {
				// Retention took the stream's first events: keep the state
				// folded before, with the version it has now.
				var kept S
				if err := json.Unmarshal([]byte(data), &kept); err != nil {
					return s, 0, false, err
				}
				return kept, version, true, nil
			}
			return s, 0, false, fmt.Errorf("events: stream %s %v lost its first events to retention and has no state to fold onto", id.s.kind, key)
		}
		e, _, err := decode(st, id.s, ev.Type, ev.Data, false)
		if err != nil {
			return s, 0, false, fmt.Errorf("events: event %d: %w", ev.Position, err)
		}
		s = d.Evolve(s, e)
		last = ev.Version
	}
	return s, last, true, rows.Err()
}

// initial returns a copy of the decider's initial state, so that an Evolve
// that changes a map or slice in place cannot change Initial.
func initial[C, E, S any](d Decider[C, E, S]) (S, error) {
	var s S
	b, err := json.Marshal(d.Initial)
	if err != nil {
		return s, fmt.Errorf("events: the decider's initial state: %w", err)
	}
	err = json.Unmarshal(b, &s)
	return s, err
}

func saveState[S any](tx *writer.Tx, kind string, key any, version int64, fold int, s S) error {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("events: the state of stream %s %v: %w", kind, key, err)
	}
	_, err = tx.Exec(`INSERT INTO lith_event_state (stream, stream_id, version, fold, state) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (stream, stream_id) DO UPDATE SET version = excluded.version, fold = excluded.fold, state = excluded.state`,
		kind, key, version, fold, string(b))
	return err
}

const insertEvent = `INSERT INTO lith_events (stream, stream_id, version, type, data, at, actor, correlation, causation)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

// appendEvents inserts the events after version and returns their
// records.
func appendEvents[K Key, E any](st *Store, tx *writer.Tx, l *local, id StreamID[K, E], key any, version int64, meta Meta, evs []E) ([]Record[K, E], error) {
	at := meta.At
	if at.IsZero() {
		at = time.Now()
	}
	ms := at.UnixMilli()
	meta.At = time.UnixMilli(ms)
	recs := make([]Record[K, E], len(evs))
	for i, e := range evs {
		name, err := id.s.nameOf(e)
		if err != nil {
			return nil, err
		}
		if up := st.upcasts[id.s.kind][name]; up != nil {
			return nil, fmt.Errorf("events: %s is read as %s: append that instead", name, up.to)
		}
		data, err := json.Marshal(e)
		if err != nil {
			return nil, fmt.Errorf("events: encode %s: %w", name, err)
		}
		v := version + int64(i) + 1
		res, err := tx.Exec(insertEvent, id.s.kind, key, v, name, string(data), ms,
			nullable(meta.Actor), nullable(meta.Correlation), nullable(meta.Causation))
		if err != nil {
			return nil, err
		}
		pos, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		recs[i] = Record[K, E]{Position: pos, ID: id.key, Version: v, Type: name, Data: e, Meta: meta}
	}
	tx.Touch(Topic)
	if wake := l.wake[id.s.kind]; wake != nil {
		tx.AfterCommit(wake)
	}
	if n := len(recs); n > 0 {
		st.committed(tx, l, id.s.kind, n, recs[n-1].Position)
	}
	return recs, nil
}

// project runs the inline projectors of the stream kind. A projector
// that rebuilds runs too, on its live table, so pages stay current, but in
// a savepoint of its own: its new handlers may not fit the old table, and
// a failure there is logged and leaves the command to go on. Its rebuild
// applies every event to the copy all the same.
func project[K Key, E any](st *Store, tx *writer.Tx, live map[string]bool, kind string, recs []Record[K, E]) error {
	for _, p := range st.inline[kind] {
		pr := p.typed.(*Projector[K, E])
		if live[p.name] {
			if err := pr.project(tx, recs); err != nil {
				return err
			}
			continue
		}
		err := tx.Savepoint(func(tx *writer.Tx) error { return pr.project(tx, recs) })
		if err != nil {
			st.local(tx).logger().Warn("events: a projector that rebuilds failed on its live table", "projector", p.name, "err", err)
		}
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
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

// Prune deletes up to limit (all for 0) of the events of stream kind s
// stored before the time before, oldest first, and returns how many it
// deleted: retention for a kind whose history need not stay, sent from a
// sweep. It keeps the state rows and the projections built from the
// events, so commands and pages go on as before; but a rebuild or a new
// projector sees only the events kept, and a decider whose Version changes
// keeps the state of a stream that lost its first events as it was. It
// deletes no event that a catch-up projector of the kind, or a rebuild
// under way, has not passed.
func Prune[K Key, E any](st *Store, tx *writer.Tx, s *Stream[K, E], before time.Time, limit int) (int64, error) {
	if _, _, err := st.prepare(tx, ""); err != nil {
		return 0, err
	}
	cutoff := int64(math.MaxInt64)
	err := tx.QueryRow(`SELECT position FROM lith_events WHERE stream = ? AND at >= ? ORDER BY position LIMIT 1`,
		s.kind, before.UnixMilli()).Scan(&cutoff)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	for _, p := range st.projectors {
		if p.kind != s.kind {
			continue
		}
		var pos int64
		var status string
		err := tx.QueryRow(`SELECT position, status FROM lith_projections WHERE name = ?`, p.name).Scan(&pos, &status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if p.catchUp || status == statusRebuilding {
			cutoff = min(cutoff, pos+1)
		}
	}
	if limit <= 0 {
		limit = -1
	}
	res, err := tx.Exec(`DELETE FROM lith_events WHERE position IN
		(SELECT position FROM lith_events WHERE stream = ? AND position < ? ORDER BY position LIMIT ?)`, s.kind, cutoff, limit)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if n > 0 {
		tx.Touch(Topic)
	}
	return n, err
}
