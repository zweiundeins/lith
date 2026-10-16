## The events

The town is 12 by 8 tiles, and each tile is a stream of lith's event log (`lith/events`), keyed by the tile's number:

```go title=events.go
// TileStream is the kind of the tiles' streams: a stream per tile, by its
// number.
var TileStream = events.NewStream[int64, TileEvent]("town.tile")
```

A change causes one of two types of event, stored as JSON in `lith_events` under their names. A third type, the snapshot, comes from the sweep ([Retention](#retention)).

```go title=events.go
// Built is something put on a tile, over what stood there.
type Built struct {
	Kind string `json:"kind"`
	Over string `json:"over,omitempty"`
}

// Cleared is a tile turned back to grass.
type Cleared struct {
	Was string `json:"was"`
}
```

Every event has a position in the log, which grows with each event and is never handed out twice. The slider moves through these positions.

## The decider

A change of a tile is a `Place` command, and the decision is a pure function of the command and what stands on the tile, folded from the tile's own events. It knows nothing of the database, so its test runs it as given, when, then (`eventstest`):

```go title=events.go
func decide(c Place, s tile) ([]TileEvent, error) {
	kind := c.Kind
	if kind == Road && (s.Kind == Water || s.Kind == Bridge) {
		kind = Bridge
	}
	switch {
	case kind == s.Kind:
		return nil, nil
	case kind == Grass:
		return []TileEvent{Cleared{Was: s.Kind}}, nil
	case (kind == House || kind == Tree) && (s.Kind == Water || s.Kind == Bridge):
		return nil, &writer.Refusal{Msg: "Only a road goes on water, as a bridge. Clear the tile first."}
	}
	return []TileEvent{Built{Kind: kind, Over: s.Kind}}, nil
}
```

`events.Execute` does the rest in the command's transaction. The store keeps each tile's state as a row of `lith_event_state`, so a change reads one row instead of the tile's history. The page sends the tile's version as it showed it, and `events.Exactly` checks it: of two people who change one tile at once, the second hears who was first, and nothing of the attempt is left.

```go title=commands.go
	err := tx.Savepoint(func(tx *writer.Tx) error {
		return events.Execute(Events, tx, PlaceTile, TileStream.ID(int64(c.Tile)), events.Exactly(c.Seen), c,
			events.Meta{At: c.At, Actor: c.Who, Causation: c.Op})
	})
	var refusal *writer.Refusal
	switch {
	case errors.As(err, &refusal):
		s.Refusal = refusal.Msg
	case errors.Is(err, events.ErrConflict):
		if s.Refusal, err = conflict(tx, c); err != nil {
			return err
		}
	case err != nil:
		return err
	}
	return demo.SaveTab(tx, Slug, c.Viewer, s, c.At)
```

The event records who acted as its actor: a hash of the session, the same that names and draws you on the shared board, never the session itself.

## Two projectors

Both projectors are inline: their handlers run in the command's transaction, after the event is stored, so no page sees an event without its effect. `town_tiles` is the town now, a row per tile with its kind, its builder and its stream's version, which the page sends back with a change. `town_history` is the town's past: a row for everything that stood on a tile, from the position of the event that put it there to the position of the one that replaced or cleared it.

```go title=events.go
var history = events.NewProjector(TileStream, "town_history", 1,
	events.Handle(TileBuilt, func(tx *writer.Tx, r events.Record[int64, Built]) error {
		if err := endRow(tx, r.ID, r.Position); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO town_history (tile, kind, who, valid_from) VALUES (?, ?, ?, ?)`,
			r.ID, r.Data.Kind, r.Actor, r.Position)
		return err
	}),
	events.Handle(TileCleared, func(tx *writer.Tx, r events.Record[int64, Cleared]) error {
		return endRow(tx, r.ID, r.Position)
	}),
	events.Handle(TileSnapshot, func(tx *writer.Tx, r events.Record[int64, Snapshot]) error {
		if err := endRow(tx, r.ID, r.Position); err != nil || r.Data.Kind == Grass {
			return err
		}
		_, err := tx.Exec(`INSERT INTO town_history (tile, kind, who, valid_from) VALUES (?, ?, ?, ?)`,
			r.ID, r.Data.Kind, r.Actor, r.Position)
		return err
	}),
).Tables("town_history")
```

Grass has no row. An `events.Runner` keeps both current: a new version of a projector rebuilds its table from the events while the pages read the old one.

## Reading the history table

The town as of position p is every row that started at or before p and had not ended by then. One query reads it, from an index that holds every column it needs:

```go title=queries.go
	rows, err := q.QueryContext(ctx, `SELECT tile, kind, who FROM town_history
		WHERE valid_from <= ?1 AND (valid_to IS NULL OR valid_to > ?1)`, p)
```

```sql title=migrations/001_town.sql
CREATE INDEX town_history_asof ON town_history (valid_from, valid_to, tile, kind, who);
```

A replay of the log up to p gives the same town, and `events.StateAt` the same tile; the demo's test checks that all three agree at every position of a random history. A replay grows with the position, the history table does not (the numbers below).

## The slider

Each tab has a position, and like all tab state on this site it is a row of `site_tabs`, written by a command: 0 is now, any other number a position of the log. While you drag, the page sends the slider's value at most once every 50 ms, the length of a tick, and once more where the thumb comes to rest. Each seek carries a number, so a seek that the network delays behind a newer one changes nothing:

```go title=expr.go
const seek = `const want = +$_town.want;
$_town.mine++;
@post('/cmd/town/seek', {payload: {tab: $_tab, at: want >= $_town.head ? 0 : want, seq: $_town.mine}, requestCancellation: 'disabled'})`
```

`Seek` stores the position and, after the commit, moves the tab's stream from the views of the old position to those of the new one, on the writer's goroutine, so the stream follows the positions in the order the writer stored them. The right end of the slider, or beyond, is now.

```go title=commands.go
	from := s.At
	s.At, s.Seq = b.clamp(c.At), c.Seq
	if s.At != from {
		s.Refusal = "" // it spoke of another moment
	}
	if err := demo.SaveTab(tx, Slug, c.Viewer, s, c.Now); err != nil {
		return err
	}
	if s.At == from {
		return nil
	}
	final, err := finalAt(tx, s.At)
	if err != nil {
		return err
	}
	tx.AfterCommit(func() { follow(c.Hub, c.Viewer.Tab, from, s.At, final) })
	return nil
```

The page shows the slider where you drag it at once, since that is your own input; the town and the events come with the frame of the position the server stored, in the next tick. Nothing is drawn ahead of the server. A pixel blinks in the town's corner while a seek is on its way.

## Views keyed by position

The town as of a position is a view of its own, `town:at:<position>`, and every tab at that position watches the same one: the hub renders it once for all of them, and keeps its frame while two or more watch, so a tab that arrives there gets it without a render. Nothing after the position changes it, so it listens to no new event, only to the sweep:

```go title=live.go
func atView(p int64) live.View {
	return live.View{
		Key:    atKey(p),
		Topics: []string{TopicPruned},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			m, err := momentAt(ctx, demo.Querier(ctx, tx), p)
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, Moment(m))
		},
	}
}
```

Around the position the page lists the event at it, the four before it and the three after it, with who did what and when. The events after a position near the end of the log are not all there yet, so they are a view of their own, `town:later:<position>`, which listens to new events only when the tab that first watched it found fewer than three. For a tab in the past, the other view that new events render again is the slider's ends, one view for every tab, which sends two numbers.

## Building in the past

A tab in the past builds nothing: its tiles take no clicks, the palette is off, and a line under the slider says to press **Now**. The badge in the stage's corner says past instead of live: the tab's stream is open, but the town it shows is not the town now. The server refuses a change from a tab whose position is not now as well. Building on the town as it was would decide on a state the town is no longer in.

## Retention

Every event is kept until 5,000 newer ones exist. Every five minutes the sweep closes the books on the older ones with `events.Compact`: each tile that loses events keeps one, its last change before the newest 5,000, rewritten in place as a snapshot of what stood on the tile then. The snapshot keeps the event's position, version, time and builder; only its type and its data change. It is the one place where lith rewrites a stored event.

```go title=events.go
// Snapshot is what stood on a tile when the sweep closed the books on its
// older events (events.Compact): it takes the place of the tile's last
// change before the newest Kept events, with its position, its time and
// who made it.
type Snapshot struct {
	Kind string `json:"kind"`
}
```

A snapshot is an event type of the tile's stream like the others. The decider's `evolve` sets the tile from it, and both projectors handle it, so a rebuild of either from the log makes the town it made before. Without the snapshots, a tile whose last change was older than the newest 5,000 events would come back as grass.

```go title=commands.go
func compact(tx *writer.Tx, keep int) error {
	from, _, err := events.Bounds(tx.Context(), tx, TileStream)
	if err != nil {
		return err
	}
	if _, err := events.Compact(Events, tx, TileSnapshot, events.Retention{Newest: keep}, PlaceTile, snapshotOf); err != nil {
		return err
	}
	first, _, err := events.Bounds(tx.Context(), tx, TileStream)
	if err != nil || first == from {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM town_history WHERE valid_to <= ?`, first); err != nil {
		return err
	}
	tx.Touch(TopicPruned)
	return nil
}
```

`events.Bounds` gives the slider its ends, with no query of the demo's own on `lith_events`. After a compaction its first position is the newest snapshot: from there on every tile reads as it stood, in the history table, in a replay of the log and in `events.StateAt`, and the demo's test checks all three after a compaction and again after a rebuild of both projectors. The history drops the rows that ended before it, which no position on the slider reaches any more. A tile keeps one snapshot at most, so the log holds 5,000 events and 96 snapshots at most. The tiles' state rows stay as they are, so changes go on deciding as before.

## Measured

On the development machine, an Intel i5-13500 under Linux, on 16 October 2026 with other work on the machine (`nice -n 19`, five runs of `go test ./internal/demos/town -bench .`), on a log of 5,000 events, 52 a tile, each look at a random position in one read transaction:

- A look at a position in the history table: 0.15 ms. A replay of the log up to the position gives the same town in 7.4 to 8.5 ms, and grows with the position; one tile's `StateAt`, 0.07 ms.
- The whole frame of a position, the town and the events up to it, is 24.8 KB of HTML. It renders in 0.22 ms with `GOGC=400`, closer to a server whose heap holds its streams; in a benchmark's small heap the collector runs all the time at its default and makes it 0.46 to 0.61 ms.
- A dragged slider costs a tick: the seek through the writer, 0.01 to 0.06 ms, and the three views the tick renders for the tab (its own, the town at the position, the events after it), 0.25 ms with `GOGC=400` and 0.61 to 0.65 ms without. Positions a few apart, through one zstd encoder as a stream has, cost about 200 bytes on the wire each.
- The writer takes 11,400 to 12,000 events a second from 96 builders at once, one per tile, each waiting for its commit: 49 changes to a transaction, each one event, a state row and both projections.
