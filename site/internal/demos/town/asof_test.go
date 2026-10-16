package town

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
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

// TestCompact keeps the newest events and closes the books on the older
// ones: the slider's start moves to the newest snapshot, the town as of
// every position from there on stays as it was, in the history table, in
// a replay of the log and in each tile's StateAt; the history keeps no row
// that ended before the start; a rebuild of both projectors makes the
// tables they made before; and changes go on deciding from the state rows.
func TestCompact(t *testing.T) {
	e := demotest.Open(t, Demo)
	build(t, e, 400, 12, 2, time.Now().Add(-time.Hour))
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
	e.Do(t, writer.Func(func(tx *writer.Tx) error { return compact(tx, 100) }))
	if got := e.Touched(); !slices.Equal(got, []string{events.Topic, TopicPruned}) {
		t.Errorf("a compaction touched %v", got)
	}
	var b Bounds
	e.View(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		if b, err = bounds(ctx, tx); err != nil {
			return err
		}
		var n, snapshots, stale int
		tx.QueryRow(`SELECT count(*), count(*) FILTER (WHERE type = 'town.snapshot') FROM lith_events`).Scan(&n, &snapshots)
		tx.QueryRow(`SELECT count(*) FROM town_history WHERE valid_to <= ?`, b.First).Scan(&stale)
		if n != 100+snapshots || snapshots == 0 || snapshots > 12 || b.Head != before.Head || b.First != before.Head-100 || stale != 0 {
			t.Fatalf("after the compaction: %d events, %d of them snapshots, %+v, %d rows ended before the first", n, snapshots, b, stale)
		}
		return checkTown(ctx, tx, b, was)
	})
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		// Before the first: the first, marked gone, its event the newest
		// snapshot.
		m, err := momentAt(ctx, tx, b.First-1)
		if err != nil {
			return err
		}
		if _, ok := m.Earlier[0].Data.(Snapshot); m.Gone != b.First-1 || m.Town != was[b.First] || m.Earlier[0].Position != b.First || !ok {
			t.Errorf("a position compacted: gone %d, first event %+v", m.Gone, m.Earlier[0])
		}
		return nil
	})

	// A rebuild of both projectors from the compacted log, as a new
	// version of them would run.
	tables := func() []string {
		var rows []string
		e.View(t, func(ctx context.Context, tx *sql.Tx) error {
			for _, q := range []string{
				`SELECT tile || ' ' || kind || ' ' || who || ' ' || version || ' ' || at FROM town_tiles ORDER BY tile`,
				`SELECT tile || ' ' || kind || ' ' || who || ' ' || valid_from || ' ' || coalesce(valid_to, 0) FROM town_history ORDER BY tile, valid_from`,
			} {
				r, err := tx.Query(q)
				if err != nil {
					return err
				}
				for r.Next() {
					var line string
					if err := r.Scan(&line); err != nil {
						return err
					}
					rows = append(rows, line)
				}
				if err := r.Close(); err != nil {
					return err
				}
			}
			return nil
		})
		return rows
	}
	made := tables()
	e.Do(t, writer.Func(func(tx *writer.Tx) error {
		_, err := tx.Exec(`UPDATE lith_projections SET version = 0 WHERE name IN ('town_tiles', 'town_history')`)
		return err
	}))
	ctx, stop := context.WithCancel(context.Background())
	runner := events.NewRunner(Events, e.DB, e.W, events.Options{Batch: 7, Log: slog.New(slog.DiscardHandler)})
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		live := 0
		for _, p := range runner.Stats().Projectors {
			if p.Status == "live" {
				live++
			}
		}
		if live == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no rebuild: %+v", runner.Stats())
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := tables(); !slices.Equal(got, made) {
		t.Errorf("after the rebuild\n%s\nbefore\n%s", strings.Join(got, "\n"), strings.Join(made, "\n"))
	}
	e.View(t, func(ctx context.Context, tx *sql.Tx) error { return checkTown(ctx, tx, b, was) })

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
			t.Errorf("tile %d after the compaction: %+v, tab %+v", i, got[i], s)
		}
	}
	// A second compaction with nothing to drop touches nothing.
	e.Touched()
	e.Do(t, writer.Func(func(tx *writer.Tx) error { return compact(tx, 200) }))
	if got := e.Touched(); len(got) != 0 {
		t.Errorf("a compaction that dropped nothing touched %v", got)
	}
}

// checkTown checks the town at every position from the slider's start to
// its end against was: from the history table, from a replay of the log,
// and every tile's StateAt.
func checkTown(ctx context.Context, tx *sql.Tx, b Bounds, was map[int64]Town) error {
	for p := b.First; p <= b.Head; p++ {
		got, err := townAt(ctx, tx, p)
		if err != nil {
			return err
		}
		replayed, err := replayAt(ctx, tx, p)
		if err != nil {
			return err
		}
		if got != was[p] || replayed != was[p] {
			return fmt.Errorf("at %d the history table has\n%v\nthe replay\n%v\nbefore\n%v", p, got, replayed, was[p])
		}
		for i := range 12 {
			s, _, err := events.StateAt(ctx, Events, tx, PlaceTile, TileStream.ID(int64(i)), p)
			if err != nil {
				return err
			}
			if s.Kind != got[i].Kind {
				return fmt.Errorf("at %d tile %d is %q, in the history table %q", p, i, s.Kind, got[i].Kind)
			}
		}
	}
	return nil
}
