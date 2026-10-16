package town

import (
	"context"
	"encoding/json"
	"time"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/sqlite"
)

// Tile is what stands on a tile: its kind, who put it there, and its
// stream's version (0 for a tile that never changed).
type Tile struct {
	Kind    string
	Who     string
	Version int64
}

// Town is every tile, by its number.
type Town [Tiles]Tile

// Bounds are the positions the slider spans, both 0 while the log is
// empty: the first where the town is whole, the newest snapshot once the
// sweep compacted it (events.Bounds), and the last.
type Bounds struct {
	First, Head int64
}

// clamp is the position a tab goes to when it asks for at: 0 (now) for
// the last position or beyond, the first for one before it.
func (b Bounds) clamp(at int64) int64 {
	switch {
	case at <= 0 || at >= b.Head:
		return 0
	case at < b.First:
		return b.First
	}
	return at
}

func bounds(ctx context.Context, q sqlite.Querier) (Bounds, error) {
	first, head, err := events.Bounds(ctx, q, TileStream)
	return Bounds{First: first, Head: head}, err
}

// townNow reads the town as it is, from town_tiles.
func townNow(ctx context.Context, q sqlite.Querier) (Town, error) {
	var t Town
	rows, err := q.QueryContext(ctx, `SELECT tile, kind, who, version FROM town_tiles`)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		var i int
		var x Tile
		if err := rows.Scan(&i, &x.Kind, &x.Who, &x.Version); err != nil {
			return t, err
		}
		if i >= 0 && i < Tiles {
			t[i] = x
		}
	}
	return t, rows.Err()
}

// townAt reads the town as of position p from the history table: the rows
// that started at or before p and had not ended by then. The index on
// (valid_from, valid_to, tile, kind, who) answers it alone.
func townAt(ctx context.Context, q sqlite.Querier, p int64) (Town, error) {
	var t Town
	rows, err := q.QueryContext(ctx, `SELECT tile, kind, who FROM town_history
		WHERE valid_from <= ?1 AND (valid_to IS NULL OR valid_to > ?1)`, p)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		var i int
		var x Tile
		if err := rows.Scan(&i, &x.Kind, &x.Who); err != nil {
			return t, err
		}
		if i >= 0 && i < Tiles {
			t[i] = x
		}
	}
	return t, rows.Err()
}

// Event is an event of the town as the page lists it.
type Event struct {
	Position int64
	Tile     int
	Who      string
	At       time.Time
	Data     TileEvent
}

// The page's lists read lith_events with queries of their own, in the
// order they list: only once bounds found events, so the table is there.

// eventsUpTo reads the n newest events at or before position p, the
// newest first.
func eventsUpTo(ctx context.Context, q sqlite.Querier, p int64, n int) ([]Event, error) {
	return readEvents(ctx, q, `SELECT position, stream_id, type, data, at, coalesce(actor, '') FROM lith_events
		WHERE stream = ? AND position <= ? ORDER BY position DESC LIMIT ?`, p, n)
}

// eventsAfter reads the n oldest events after position p, the newest
// first.
func eventsAfter(ctx context.Context, q sqlite.Querier, p int64, n int) ([]Event, error) {
	evs, err := readEvents(ctx, q, `SELECT position, stream_id, type, data, at, coalesce(actor, '') FROM lith_events
		WHERE stream = ? AND position > ? ORDER BY position LIMIT ?`, p, n)
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
	return evs, err
}

// hasLater reports whether n events follow position p: then the events
// after p that the page lists are final, and no new event changes them.
func hasLater(ctx context.Context, q sqlite.Querier, p int64, n int) (bool, error) {
	var c int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM lith_events WHERE stream = ? AND position > ? LIMIT ?)`,
		TileStream.Kind(), p, n).Scan(&c)
	return c == n, err
}

func readEvents(ctx context.Context, q sqlite.Querier, query string, p int64, n int) ([]Event, error) {
	rows, err := q.QueryContext(ctx, query, TileStream.Kind(), p, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Event
	for rows.Next() {
		var e Event
		var typ, data string
		var at int64
		if err := rows.Scan(&e.Position, &e.Tile, &typ, &data, &at, &e.Who); err != nil {
			return nil, err
		}
		if e.Data, err = events.Decode(Events, TileStream, typ, json.RawMessage(data)); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at)
		list = append(list, e)
	}
	return list, rows.Err()
}
