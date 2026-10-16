package town

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demos/board"
)

// Hub is what the commands ask of lith's live hub: to change the views a
// tab's stream watches. The handlers hand them the kit's.
type Hub interface {
	Watch(streamID string, v live.View) bool
	Unwatch(streamID, key string) bool
}

var _ Hub = (*live.Hub)(nil)

// Place puts a kind of thing on a tile, or clears it, if the tile is
// still at the version its page showed, and records the outcome in the
// state of the tab that sent it. A tab that shows the past changes
// nothing.
type Place struct {
	Viewer demo.Viewer
	// Who is the builder's avatar seed (board.WhoOf of the session).
	Who  string
	Op   string
	Tile int
	Kind string
	// Seen is the tile's version on the builder's page.
	Seen int64
	At   time.Time
}

func (c Place) Validate() error {
	switch {
	case c.Viewer.Tab == "" || c.Op == "":
		return errors.New("a change needs a tab and an op")
	case c.Tile < 0 || c.Tile >= Tiles:
		return fmt.Errorf("a tile is 0 to %d", Tiles-1)
	case !slices.Contains(Tools, c.Kind):
		return errors.New("that is nothing a tile can hold")
	case c.Seen < 0:
		return errors.New("a change needs the tile's version")
	}
	return nil
}

func (c Place) Apply(tx *writer.Tx) error {
	var s TabState
	if err := demo.LoadTab(tx.Context(), tx, Slug, c.Viewer, &s); err != nil {
		return err
	}
	s.Op, s.Tile, s.Refusal = c.Op, c.Tile, ""
	if s.At != 0 {
		s.Refusal = "This tab shows the past. Press Now to build."
		return demo.SaveTab(tx, Slug, c.Viewer, s, c.At)
	}
	// A refusal or a conflict leaves nothing of the attempt: the savepoint
	// takes it back, and the tab hears why.
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
}

// conflict says who changed the tile after the builder's page drew it,
// and what stands there now.
func conflict(tx *writer.Tx, c Place) (string, error) {
	var kind, who string
	err := tx.QueryRow(`SELECT kind, who FROM town_tiles WHERE tile = ?`, c.Tile).Scan(&kind, &who)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	by := nameOf(who) + " changed " + label(c.Tile)
	if who == c.Who {
		by = "You changed " + label(c.Tile) + " in another tab"
	}
	return by + " first. It is " + kindPhrase(kind) + " now.", nil
}

// Seek moves a tab to a position of the log, or to now (At 0, or the last
// position), and its stream to the views of that position: the views it
// leaves are unwatched, the new ones watched, after the commit, on the
// writer's goroutine, so they follow the positions in the order the writer
// stored them. Seq numbers a page's seeks: one older than the tab's last,
// which the network delayed, changes nothing.
type Seek struct {
	Viewer demo.Viewer
	At     int64
	Seq    int64
	Now    time.Time
	Hub    Hub
}

func (c Seek) Validate() error {
	if c.Viewer.Tab == "" || c.At < 0 || c.Seq < 1 {
		return errors.New("a seek needs a tab, a position and its number")
	}
	return nil
}

func (c Seek) Apply(tx *writer.Tx) error {
	var s TabState
	if err := demo.LoadTab(tx.Context(), tx, Slug, c.Viewer, &s); err != nil {
		return err
	}
	if c.Seq <= s.Seq {
		return nil
	}
	b, err := bounds(tx.Context(), tx)
	if err != nil {
		return err
	}
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
}

// finalAt reports whether the events after position at that the page
// lists are all there (hasLater); at now there are none to list.
func finalAt(tx *writer.Tx, at int64) (bool, error) {
	if at == 0 {
		return false, nil
	}
	return hasLater(tx.Context(), tx, at, Later)
}

// Follow watches the views of the tab's position: a stream's first frame
// asks for it, so that a new stream, the first or one after a deploy,
// shows the position its tab is at.
type Follow struct {
	Viewer demo.Viewer
	Hub    Hub
}

func (c Follow) Validate() error {
	if c.Viewer.Tab == "" {
		return errors.New("a hello needs a tab")
	}
	return nil
}

func (c Follow) Apply(tx *writer.Tx) error {
	var s TabState
	if err := demo.LoadTab(tx.Context(), tx, Slug, c.Viewer, &s); err != nil {
		return err
	}
	final, err := finalAt(tx, s.At)
	if err != nil {
		return err
	}
	tx.AfterCommit(func() { watch(c.Hub, c.Viewer.Tab, s.At, final) })
	return nil
}

// follow moves a tab's stream from the views of one position to those of
// another. final says that the events after the new position are all
// there (hasLater).
func follow(h Hub, tab string, from, to int64, final bool) {
	if h == nil {
		return
	}
	for _, key := range keysAt(from) {
		h.Unwatch(tab, key)
	}
	watch(h, tab, to, final)
}

func watch(h Hub, tab string, at int64, final bool) {
	if h == nil {
		return
	}
	for _, v := range viewsAt(at, final) {
		h.Watch(tab, v)
	}
}

// start is the town's first layout: a river with a bridge, a road, a few
// houses and trees, laid out once by the town itself.
var start = []struct {
	tile int
	kind string
}{
	{8, Water}, {20, Water}, {32, Water}, {44, Water}, {56, Water}, {68, Water}, {67, Water}, {79, Water}, {91, Water},
	{36, Road}, {37, Road}, {38, Road}, {39, Road}, {40, Road}, {41, Road}, {42, Road}, {43, Road}, {44, Road}, {45, Road}, {46, Road}, {47, Road},
	{25, House}, {27, House}, {49, House}, {51, House}, {58, House},
	{2, Tree}, {13, Tree}, {14, Tree}, {11, Tree}, {34, Tree}, {61, Tree}, {74, Tree}, {75, Tree}, {86, Tree}, {82, Tree},
}

// Seed lays out the starting town as events, once.
func Seed(now time.Time) writer.Func {
	return func(tx *writer.Tx) error {
		var seeded int
		if err := tx.QueryRow(`SELECT seeded FROM town_meta WHERE id = 1`).Scan(&seeded); err != nil || seeded == 1 {
			return err
		}
		for _, s := range start {
			err := events.Execute(Events, tx, PlaceTile, TileStream.ID(int64(s.tile)), events.Any,
				Place{Tile: s.tile, Kind: s.kind}, events.Meta{At: now})
			if err != nil {
				return err
			}
		}
		_, err := tx.Exec(`UPDATE town_meta SET seeded = 1 WHERE id = 1`)
		return err
	}
}

// sweep keeps the newest Kept events.
func sweep(tx *writer.Tx, now time.Time) error { return prune(tx, now, Kept) }

// prune keeps the newest keep events. events.Prune keeps each tile's
// state row, so changes go on deciding as before, and town_tiles as it
// is; the history drops the rows that ended at or before the oldest event
// kept, which no position on the slider reaches any more.
func prune(tx *writer.Tx, now time.Time, keep int64) error {
	var n int64
	if err := tx.QueryRow(`SELECT count(*) FROM lith_events WHERE stream = ?`, TileStream.Kind()).Scan(&n); err != nil {
		if noEvents(err) {
			return nil
		}
		return err
	}
	if n <= keep {
		return nil
	}
	if _, err := events.Prune(Events, tx, TileStream, now, int(n-keep)); err != nil {
		return err
	}
	b, err := bounds(tx.Context(), tx)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM town_history WHERE valid_to <= ?`, b.First); err != nil {
		return err
	}
	tx.Touch(TopicPruned)
	return nil
}

// whoOf is the seed of a session's avatar, the board's: the same person
// has the same name and face on both.
func whoOf(session string) string { return board.WhoOf(session) }
