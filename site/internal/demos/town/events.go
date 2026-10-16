package town

import (
	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/writer"
)

// The kinds of tile. Grass is the empty tile; a bridge is what a road
// becomes on water.
const (
	Grass  = ""
	House  = "house"
	Tree   = "tree"
	Road   = "road"
	Water  = "water"
	Bridge = "bridge"
)

// Tools are what a person can put on a tile, in the palette's order;
// Grass clears it.
var Tools = []string{House, Tree, Road, Water, Grass}

// TileEvent is an event of a tile's stream.
type TileEvent interface{ tileEvent() }

// Built is something put on a tile, over what stood there.
type Built struct {
	Kind string `json:"kind"`
	Over string `json:"over,omitempty"`
}

// Cleared is a tile turned back to grass.
type Cleared struct {
	Was string `json:"was"`
}

// Snapshot is what stood on a tile when the sweep closed the books on its
// older events (events.Compact): it takes the place of the tile's last
// change before the newest Kept events, with its position, its time and
// who made it.
type Snapshot struct {
	Kind string `json:"kind"`
}

func (Built) tileEvent()    {}
func (Cleared) tileEvent()  {}
func (Snapshot) tileEvent() {}

// TileStream is the kind of the tiles' streams: a stream per tile, by its
// number.
var TileStream = events.NewStream[int64, TileEvent]("town.tile")

func defineTile[T TileEvent](name string) events.Type[int64, TileEvent, T] {
	return events.Define(TileStream, name, func(t T) TileEvent { return t })
}

var (
	TileBuilt    = defineTile[Built]("town.built")
	TileCleared  = defineTile[Cleared]("town.cleared")
	TileSnapshot = defineTile[Snapshot]("town.snapshot")
)

// tile is what a change decides on: what stands on the tile, folded from
// its stream.
type tile struct {
	Kind string `json:"kind"`
}

// PlaceTile is the decider of Place.
var PlaceTile = events.Decider[Place, TileEvent, tile]{
	Decide:  decide,
	Evolve:  evolve,
	Version: 1,
}

// decide turns a change into the event it causes: nothing when the tile
// holds that already, a bridge for a road on water, and no house or tree
// on water.
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

func evolve(s tile, e TileEvent) tile {
	switch e := e.(type) {
	case Built:
		s.Kind = e.Kind
	case Cleared:
		s.Kind = Grass
	case Snapshot:
		s.Kind = e.Kind
	}
	return s
}

// snapshotOf is a tile's snapshot of its state.
func snapshotOf(s tile) Snapshot { return Snapshot{Kind: s.Kind} }

// tiles keeps town_tiles, the town now: every event writes its tile's row
// and touches Topic, which the views of the town now and of the slider's
// end listen to.
var tiles = events.NewProjector(TileStream, "town_tiles", 1,
	events.Handle(TileBuilt, func(tx *writer.Tx, r events.Record[int64, Built]) error {
		return setTile(tx, r.ID, r.Data.Kind, r.Actor, r.Version, r.At.UnixMilli())
	}),
	events.Handle(TileCleared, func(tx *writer.Tx, r events.Record[int64, Cleared]) error {
		return setTile(tx, r.ID, Grass, r.Actor, r.Version, r.At.UnixMilli())
	}),
	events.Handle(TileSnapshot, func(tx *writer.Tx, r events.Record[int64, Snapshot]) error {
		return setTile(tx, r.ID, r.Data.Kind, r.Actor, r.Version, r.At.UnixMilli())
	}),
).Tables("town_tiles")

func setTile(tx *writer.Tx, id int64, kind, who string, version, at int64) error {
	_, err := tx.Exec(`INSERT INTO town_tiles (tile, kind, who, version, at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (tile) DO UPDATE SET kind = excluded.kind, who = excluded.who, version = excluded.version, at = excluded.at`,
		id, kind, who, version, at)
	tx.Touch(Topic)
	return err
}

// history keeps town_history, every tile's past: an event ends the row of
// what stood on the tile at the event's position, and a Built, or a
// snapshot of something, starts the next row there. It touches nothing:
// no view of a position before the event changes, and the town now is
// town_tiles.
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

// endRow ends the row of what stands on a tile at position pos.
func endRow(tx *writer.Tx, id, pos int64) error {
	_, err := tx.Exec(`UPDATE town_history SET valid_to = ? WHERE tile = ? AND valid_to IS NULL`, pos, id)
	return err
}

// Events is the town's event store. The site runs an events.Runner on it,
// which rebuilds a projector whose version changed.
var Events = events.Must(events.New(tiles, history))
