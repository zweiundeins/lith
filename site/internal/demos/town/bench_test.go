package town

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demo/demotest"
)

// fill appends n events, a change of each tile in turn, every change one
// event: house, tree, road and grass again, so a tile's history has a row
// for three of every four events. Then it checkpoints the WAL, as the
// writer does once it has been idle for a second: reads from a long WAL
// cost twice as much.
func fill(b *testing.B, e *demotest.Env, n int) {
	b.Helper()
	cycle := []string{House, Tree, Road, Grass}
	err := e.W.Do(context.Background(), writer.Func(func(tx *writer.Tx) error {
		for i := range n {
			c := Place{Tile: i % Tiles, Kind: cycle[i/Tiles%len(cycle)]}
			err := events.Execute(Events, tx, PlaceTile, TileStream.ID(int64(c.Tile)), events.Any, c,
				events.Meta{Actor: "0a1b2c3d", At: time.Now()})
			if err != nil {
				return err
			}
		}
		return nil
	}))
	if err == nil {
		err = e.W.Exclusive(context.Background(), func(ctx context.Context, conn *sql.Conn) error {
			_, err := conn.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
			return err
		})
	}
	if err != nil {
		b.Fatal(err)
	}
}

// BenchmarkLook is a tab that looks back to a random position of a log
// of Kept events: the town from the history table, the whole frame of
// that position (the town, the events up to it), the same town from a
// replay of the log, and one tile's state from its stream. Each op is
// one read transaction, as a render has. A frame allocates 200 KB, which
// in a benchmark's small heap keeps the collector running; GOGC=400 is
// closer to a server, whose heap holds its streams.
//
//	go test ./internal/demos/town -run '^$' -bench Look
//	GOGC=400 go test ./internal/demos/town -run '^$' -bench 'Look|Drag'
func BenchmarkLook(b *testing.B) {
	e := demotest.Open(b, Demo)
	fill(b, e, Kept)
	ctx := context.Background()
	for _, c := range []struct {
		name string
		look func(tx *sql.Tx, p int64) (int, error)
	}{
		{"history-table", func(tx *sql.Tx, p int64) (int, error) { _, err := townAt(ctx, tx, p); return 0, err }},
		{"frame", func(tx *sql.Tx, p int64) (int, error) {
			f, err := atView(p).Render(ctx, tx)
			if err != nil {
				return 0, err
			}
			return len(f.([]byte)), nil
		}},
		{"replay", func(tx *sql.Tx, p int64) (int, error) { _, err := replayAt(ctx, tx, p); return 0, err }},
		{"state-of-one-tile", func(tx *sql.Tx, p int64) (int, error) {
			_, _, err := events.StateAt(ctx, Events, tx, PlaceTile, TileStream.ID(p%Tiles), p)
			return 0, err
		}},
	} {
		b.Run(c.name, func(b *testing.B) {
			rnd := rand.New(rand.NewPCG(1, 2))
			bytes := 0
			for b.Loop() {
				p := 1 + rnd.Int64N(Kept)
				err := e.DB.View(ctx, func(tx *sql.Tx) error {
					n, err := c.look(tx, p)
					bytes += n
					return err
				})
				if err != nil {
					b.Fatal(err)
				}
			}
			if bytes > 0 {
				b.ReportMetric(float64(bytes)/float64(b.N), "B/frame")
			}
		})
	}
}

// BenchmarkDrag is a tab whose slider is dragged, a position a tick: the
// seek through the writer, which stores the tab's position and moves its
// stream (here to no hub), and the views that tick renders for the tab:
// its own, the town at the position and the events after it.
//
//	go test ./internal/demos/town -run '^$' -bench Drag
func BenchmarkDrag(b *testing.B) {
	e := demotest.Open(b, Demo)
	fill(b, e, Kept)
	ctx := context.Background()
	v := demo.Viewer{Session: "s1", Tab: "tab1"}
	b.Run("seek", func(b *testing.B) {
		rnd := rand.New(rand.NewPCG(3, 4))
		seq := int64(0)
		for b.Loop() {
			seq++
			if err := e.W.Do(ctx, Seek{Viewer: v, At: 1 + rnd.Int64N(Kept-1), Seq: seq, Now: time.Now()}); err != nil {
				b.Fatal(err)
			}
		}
	})
	// What a step costs on the wire: the frames of a drag, a position a
	// tick a few positions apart, through one zstd encoder as a stream
	// has (the fastest level, a window of 2^17), each flushed.
	b.Run("wire", func(b *testing.B) {
		var out countWriter
		enc, err := zstd.NewWriter(&out, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(1<<17), zstd.WithEncoderConcurrency(1))
		if err != nil {
			b.Fatal(err)
		}
		rnd := rand.New(rand.NewPCG(7, 8))
		p, raw := int64(Kept/2), 0
		for b.Loop() {
			p = max(1, min(Kept-1, p+rnd.Int64N(9)-4))
			err := e.DB.View(ctx, func(tx *sql.Tx) error {
				f, err := atView(p).Render(ctx, tx)
				if err != nil {
					return err
				}
				raw += len(f.([]byte))
				enc.Write(f.([]byte))
				return enc.Flush()
			})
			if err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(raw)/float64(b.N), "B/frame")
		b.ReportMetric(float64(out)/float64(b.N), "B/step")
	})
	b.Run("render", func(b *testing.B) {
		rnd := rand.New(rand.NewPCG(5, 6))
		for b.Loop() {
			p := 1 + rnd.Int64N(Kept-1)
			err := e.DB.View(ctx, func(tx *sql.Tx) error {
				for _, view := range []live.View{tabView(v), atView(p), laterView(p, true)} {
					if _, err := view.Render(ctx, tx); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

type countWriter int

func (w *countWriter) Write(p []byte) (int, error) {
	*w += countWriter(len(p))
	return len(p), nil
}

// BenchmarkPlace measures the events a second the writer takes: a builder
// per tile, 96 at once, each sending Place commands one after the other
// and waiting for each commit, as the handlers do; each change appends an
// event and runs both projectors.
//
//	go test ./internal/demos/town -run '^$' -bench Place
func BenchmarkPlace(b *testing.B) {
	e := demotest.Open(b, Demo)
	var sent atomic.Int64
	var wg sync.WaitGroup
	b.ResetTimer()
	for id := range Tiles {
		wg.Go(func() {
			v := demo.Viewer{Session: fmt.Sprint("s", id), Tab: fmt.Sprint("tab", id)}
			for seen := int64(0); sent.Add(1) <= int64(b.N); seen++ {
				kind := House
				if seen%2 == 1 {
					kind = Tree
				}
				c := Place{Viewer: v, Who: "0a1b2c3d", Op: fmt.Sprint("op", seen), Tile: id, Kind: kind, Seen: seen, At: time.Now()}
				if err := e.W.Do(context.Background(), c); err != nil {
					b.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	b.StopTimer()
	var n int64
	e.DB.R.QueryRow(`SELECT count(*) FROM lith_events`).Scan(&n)
	if n != int64(b.N) {
		b.Errorf("%d events of %d changes", n, b.N)
	}
	b.ReportMetric(float64(n)/b.Elapsed().Seconds(), "events/s")
	st := e.W.Stats()
	b.ReportMetric(float64(st.Commands)/float64(max(1, st.Batches)), "commands/batch")
}
