package town

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demo/demotest"
)

// build makes a random history of n changes on the first tiles of the
// town, each a Place command, by three people; refused changes and those
// that change nothing leave no event.
func build(t testing.TB, e *demotest.Env, n, tiles int, seed uint64, at time.Time) {
	t.Helper()
	rnd := rand.New(rand.NewPCG(seed, 7))
	people := []demo.Viewer{ada, bob, {Session: "s-cy", Tab: "tab-cy"}}
	err := e.W.Do(context.Background(), writer.Func(func(tx *writer.Tx) error {
		for i := range n {
			v := people[rnd.IntN(len(people))]
			c := Place{Viewer: v, Who: whoOf(v.Session), Op: fmt.Sprint("op", i), Tile: rnd.IntN(tiles), Kind: Tools[rnd.IntN(len(Tools))], At: at.Add(time.Duration(i) * time.Millisecond)}
			var version int64
			if err := tx.QueryRow(`SELECT coalesce((SELECT version FROM town_tiles WHERE tile = ?), 0)`, c.Tile).Scan(&version); err != nil {
				return err
			}
			c.Seen = version
			if err := c.Apply(tx); err != nil {
				return err
			}
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
}

// replayAt folds every event of the log up to position p into each tile.
func replayAt(ctx context.Context, q sqlite.Querier, p int64) (Town, error) {
	var town Town
	for raw, err := range events.SinceUntil(ctx, q, 0, p) {
		if err != nil {
			return town, err
		}
		e, err := events.Decode(Events, TileStream, raw.Type, raw.Data)
		if err != nil {
			return town, err
		}
		i := raw.ID.(int64)
		town[i].Kind = evolve(tile{Kind: town[i].Kind}, e).Kind
		town[i].Who = raw.Actor
		if town[i].Kind == Grass {
			town[i].Who = ""
		}
	}
	return town, nil
}

// kinds is a town without its versions, as the history table has it.
func kinds(t Town) Town {
	for i := range t {
		t[i].Version = 0
		if t[i].Kind == Grass {
			t[i].Who = ""
		}
	}
	return t
}

// TestAsOf checks the history projector at every position of a random
// history: the town from the history table, the town from a replay of the
// log up to the position, and every tile's state from its own stream
// (StateAt) agree; and at the last position, the history agrees with
// town_tiles, the other projector.
func TestAsOf(t *testing.T) {
	e := demotest.Open(t, Demo)
	build(t, e, 600, 16, 1, time.Now())
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		b, err := bounds(ctx, tx)
		if err != nil {
			return err
		}
		if b.First != 1 || b.Head < 300 {
			t.Fatalf("a history of %+v", b)
		}
		for p := b.First; p <= b.Head; p++ {
			table, err := townAt(ctx, tx, p)
			if err != nil {
				return err
			}
			replayed, err := replayAt(ctx, tx, p)
			if err != nil {
				return err
			}
			if table != replayed {
				return fmt.Errorf("at %d the history table has\n%v\nthe replay\n%v", p, table, replayed)
			}
			for i := range 16 {
				s, _, err := events.StateAt(ctx, Events, tx, PlaceTile, TileStream.ID(int64(i)), p)
				if err != nil {
					return err
				}
				if s.Kind != table[i].Kind {
					return fmt.Errorf("at %d tile %d is %q, in the history table %q", p, i, s.Kind, table[i].Kind)
				}
			}
		}
		now, err := townNow(ctx, tx)
		if err != nil {
			return err
		}
		last, err := townAt(ctx, tx, b.Head)
		if err != nil {
			return err
		}
		if kinds(now) != last {
			t.Errorf("town_tiles\n%v\nthe history at the end\n%v", kinds(now), last)
		}
		return nil
	})
}

// TestPrune keeps the newest events: the slider's start moves to the
// oldest kept, the town as of every kept position stays as it was, the
// history keeps no row that ended before it, and changes go on deciding
// from the state rows.
func TestPrune(t *testing.T) {
	e := demotest.Open(t, Demo)
	start := time.Now().Add(-time.Hour)
	build(t, e, 400, 12, 2, start)
	var before Bounds
	was := map[int64]Town{}
	e.View(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		if before, err = bounds(ctx, tx); err != nil {
			return err
		}
		for p := before.First; p <= before.Head; p++ {
			if was[p], err = townAt(ctx, tx, p); err != nil {
				return err
			}
		}
		return nil
	})
	e.Touched()
	e.Do(t, writer.Func(func(tx *writer.Tx) error { return prune(tx, time.Now(), 100) }))
	if got := e.Touched(); len(got) != 2 || got[1] != TopicPruned {
		t.Errorf("a prune touched %v", got)
	}
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		b, err := bounds(ctx, tx)
		if err != nil {
			return err
		}
		var n, stale int
		tx.QueryRow(`SELECT count(*) FROM lith_events`).Scan(&n)
		tx.QueryRow(`SELECT count(*) FROM town_history WHERE valid_to <= ?`, b.First).Scan(&stale)
		if n != 100 || b.Head != before.Head || b.First != before.Head-99 || stale != 0 {
			t.Fatalf("after the prune: %d events, %+v, %d rows ended before the first", n, b, stale)
		}
		for p := b.First; p <= b.Head; p++ {
			got, err := townAt(ctx, tx, p)
			if err != nil {
				return err
			}
			if got != was[p] {
				return fmt.Errorf("at %d after the prune\n%v\nbefore\n%v", p, got, was[p])
			}
		}
		// Before the first: the oldest kept, marked gone.
		m, err := momentAt(ctx, tx, b.First-1)
		if err != nil {
			return err
		}
		if m.Gone != b.First-1 || m.Town != was[b.First] || m.Earlier[0].Position != b.First {
			t.Errorf("a position pruned: gone %d, first event %d", m.Gone, m.Earlier[0].Position)
		}
		return nil
	})
	// The tiles decide from their state rows, whose first events are gone.
	town, _ := readTown(t, e, ada)
	for i, x := range town[:12] {
		kind := House
		if x.Kind == House || x.Kind == Water || x.Kind == Bridge {
			kind = Grass
		}
		e.Do(t, change(ada, fmt.Sprint("after", i), i, kind, x.Version))
		got, s := readTown(t, e, ada)
		if got[i].Kind != kind || s.Refusal != "" {
			t.Errorf("tile %d after the prune: %+v, tab %+v", i, got[i], s)
		}
	}
	// A second prune with nothing to drop touches nothing.
	e.Touched()
	e.Do(t, writer.Func(func(tx *writer.Tx) error { return prune(tx, time.Now(), 200) }))
	if got := e.Touched(); len(got) != 0 {
		t.Errorf("a prune that dropped nothing touched %v", got)
	}
}
