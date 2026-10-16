package events

import (
	"context"
	"database/sql"
	"fmt"
	"iter"
	"math"
	"strings"
	"time"

	"github.com/zweiundeins/lith/sqlite"
)

// Load yields the events of a stream after version after (0 for all), in
// order, decoded and upcast, read through q: a GET's transaction, a live
// Render's sqlite.QuerierFrom(ctx), a command's *writer.Tx. Breaking off
// the loop closes the rows. A render reads a projection rather than
// replaying a stream, whose history only grows; Load is for a stream's
// own history page, a test, a tool. After Compact, a stream that lost
// events starts at its snapshot. A database whose tables the first
// command has not made yet has no events.
func Load[K Key, E any](ctx context.Context, st *Store, q sqlite.Querier, id StreamID[K, E], after int64) iter.Seq2[Record[K, E], error] {
	return LoadUntil(ctx, st, q, id, after, math.MaxInt64)
}

// LoadUntil is Load up to a position: the events of a stream after
// version after that were stored at or before position upto, the stream
// as it stood then.
func LoadUntil[K Key, E any](ctx context.Context, st *Store, q sqlite.Querier, id StreamID[K, E], after, upto int64) iter.Seq2[Record[K, E], error] {
	return func(yield func(Record[K, E], error) bool) {
		key := keyValue(id.key)
		rows, err := q.QueryContext(ctx, `SELECT position, version, type, data, at, actor, correlation, causation
			FROM lith_events WHERE stream = ? AND stream_id = ? AND version > ? AND position <= ? ORDER BY version`, id.s.kind, key, after, upto)
		if err != nil {
			if !noTable(err) {
				yield(Record[K, E]{}, err)
			}
			return
		}
		defer rows.Close()
		for rows.Next() {
			ev, err := scanRaw(rows, id.s.kind, key)
			if err != nil {
				yield(Record[K, E]{}, err)
				return
			}
			if !yield(record(st, id.s, ev)) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(Record[K, E]{}, err)
		}
	}
}

// StateAt folds a stream's events up to position upto into d's state:
// what a command on the stream decided from then, for a page that shows
// the past. It returns the stream's version then too. It reads through q
// and stores nothing, and it folds from the stream's first event, or its
// snapshot after Compact, so it costs what the stream's history up to
// upto costs to read. It fails where retention left no state to fold: a
// stream whose first events Prune took, or a position before the stream's
// snapshot.
func StateAt[K Key, C, E, S any](ctx context.Context, st *Store, q sqlite.Querier, d Decider[C, E, S], id StreamID[K, E], upto int64) (S, int64, error) {
	s, err := initial(d)
	if err != nil {
		return s, 0, err
	}
	var version, first, from int64
	for r, err := range LoadUntil(ctx, st, q, id, 0, upto) {
		if err != nil {
			return s, 0, err
		}
		if version == 0 {
			first, from = r.Version, r.Position
		}
		s, version = d.Evolve(s, r.Data), r.Version
	}
	key := keyValue(id.key)
	switch {
	case version == 0:
		// No event up to upto: the stream began later, or retention took
		// what it had then.
		err := q.QueryRowContext(ctx, `SELECT coalesce(min(version), 0) FROM lith_events WHERE stream = ? AND stream_id = ?`,
			id.s.kind, key).Scan(&first)
		if err != nil && !noTable(err) {
			return s, 0, err
		}
		if first > 1 {
			return s, 0, fmt.Errorf("events: retention took the events of stream %s %v up to position %d", id.s.kind, key, upto)
		}
	case first != 1:
		// A snapshot, or what Prune left.
		through, err := compacted(ctx, q, id.s.kind)
		if err != nil {
			return s, 0, err
		}
		if from > through {
			return s, 0, fmt.Errorf("events: stream %s %v lost its first events to retention", id.s.kind, key)
		}
	}
	return s, version, nil
}

// Bounds returns the positions of the first and the last event of stream
// kind s, 0 and 0 without any: the ends of a slider over its history.
// After Compact, first is the newest snapshot, the oldest position at
// which every stream of the kind reads as it stood; the snapshots before
// it are of streams that did not change between them and it. It reads
// through q, with a seek in the log's index for each end.
func Bounds[K Key, E any](ctx context.Context, q sqlite.Querier, s *Stream[K, E]) (first, last int64, err error) {
	err = q.QueryRowContext(ctx, `SELECT
		coalesce((SELECT position FROM lith_events WHERE stream = ?1
			AND position >= coalesce((SELECT compacted FROM lith_event_kinds WHERE stream = ?1), 0)
			ORDER BY position LIMIT 1), 0),
		coalesce((SELECT position FROM lith_events WHERE stream = ?1 ORDER BY position DESC LIMIT 1), 0)`, s.kind).Scan(&first, &last)
	if err != nil && noTable(err) {
		return 0, 0, nil
	}
	return first, last, err
}

// Since yields every event stored after position (0 for all), of every
// stream, in the order they were stored, as the table holds them: an
// activity feed is a view over it that lists Topic. Positions only grow,
// and they commit in order, so a reader that keeps the last position it
// saw misses no event. Breaking off the loop closes the rows. A database
// whose tables the first command has not made yet has no events.
func Since(ctx context.Context, q sqlite.Querier, position int64) iter.Seq2[Raw, error] {
	return SinceUntil(ctx, q, position, math.MaxInt64)
}

// SinceUntil is Since up to a position: every event stored after position
// after and at or before position upto.
func SinceUntil(ctx context.Context, q sqlite.Querier, after, upto int64) iter.Seq2[Raw, error] {
	return func(yield func(Raw, error) bool) {
		rows, err := q.QueryContext(ctx, `SELECT position, version, type, data, at, actor, correlation, causation, stream, stream_id
			FROM lith_events WHERE position > ? AND position <= ? ORDER BY position`, after, upto)
		if err != nil {
			if !noTable(err) {
				yield(Raw{}, err)
			}
			return
		}
		defer rows.Close()
		for rows.Next() {
			if !yield(scanRaw(rows, "", nil)) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(Raw{}, err)
		}
	}
}

// page reads up to n events of a stream kind after position.
func page(ctx context.Context, q sqlite.Querier, kind string, position int64, n int) ([]Raw, error) {
	rows, err := q.QueryContext(ctx, `SELECT position, version, type, data, at, actor, correlation, causation, stream, stream_id
		FROM lith_events WHERE stream = ? AND position > ? ORDER BY position LIMIT ?`, kind, position, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var evs []Raw
	for rows.Next() {
		ev, err := scanRaw(rows, "", nil)
		if err != nil {
			return nil, err
		}
		evs = append(evs, ev)
	}
	return evs, rows.Err()
}

// scanRaw scans a row of position, version, type, data, at, actor,
// correlation and causation, then stream and stream_id unless kind is
// given.
func scanRaw(rows *sql.Rows, kind string, key any) (Raw, error) {
	var ev Raw
	var data string
	var at int64
	var actor, correlation, causation sql.NullString
	dest := []any{&ev.Position, &ev.Version, &ev.Type, &data, &at, &actor, &correlation, &causation}
	if kind == "" {
		dest = append(dest, &ev.Stream, &ev.ID)
	} else {
		ev.Stream, ev.ID = kind, key
	}
	if err := rows.Scan(dest...); err != nil {
		return ev, err
	}
	ev.Data = []byte(data)
	ev.Meta = Meta{At: time.UnixMilli(at), Actor: actor.String, Correlation: correlation.String, Causation: causation.String}
	return ev, nil
}

// noTable reports an error of a read before the first command made the
// tables: such a database has no events.
func noTable(err error) bool {
	return strings.Contains(err.Error(), "no such table: lith_event")
}
