package events_test

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"
)

// BenchmarkExecute is events through the writer: 1, 50 or 200 producers,
// each renaming ten cards of its own with Execute and waiting for the
// commit (Do), as a request handler does. A rename reads the card's state
// row, decides, appends one event, saves the row and runs the titles
// projector inline, which updates card_titles: five statements, plus the
// writer's SAVEPOINT and RELEASE, against BenchmarkWriter's four.
//
//	go test ./events -run '^$' -bench Execute -benchtime 5s
func BenchmarkExecute(b *testing.B) {
	for _, sync := range []string{"FULL", "NORMAL"} {
		for _, producers := range []int{1, 50, 200} {
			b.Run(fmt.Sprintf("%s/producers=%d", sync, producers), func(b *testing.B) {
				benchRenames(b, sync, producers)
			})
		}
	}
}

func benchRenames(b *testing.B, synchronous string, producers int) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(b.TempDir(), "bench.db"), sqlite.Options{
		Synchronous: synchronous,
		Migrations:  fstest.MapFS{"001_cards.sql": {Data: []byte(schema)}},
	})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	w, err := writer.New(db, writer.Options{Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		b.Fatal(err)
	}
	go w.Run(ctx)
	defer w.Close()
	st := events.Must(events.New(titles(1, same)))

	const cards = 10
	err = w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
		for id := range int64(producers * cards) {
			err := events.Execute(st, tx, create, Cards.ID(id+1), events.NoStream, Create{Title: "card"}, events.Meta{})
			if err != nil {
				return err
			}
		}
		return nil
	}))
	if err != nil {
		b.Fatal(err)
	}

	var n atomic.Int64
	s0 := w.Stats()
	b.ResetTimer()
	var wg sync.WaitGroup
	for p := range producers {
		wg.Go(func() {
			first := int64(p*cards + 1)
			for i := n.Add(1); i <= int64(b.N); i = n.Add(1) {
				id := first + i%cards
				err := w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
					return events.Execute(st, tx, rename, Cards.ID(id), events.Any, Rename{Title: fmt.Sprint("title ", i)},
						events.Meta{Actor: "bench"})
				}))
				if err != nil {
					b.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	b.StopTimer()
	s := w.Stats()
	batches := float64(s.Batches - s0.Batches)
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "events/s")
	b.ReportMetric(float64(s.Commands-s0.Commands)/batches, "cmds/tx")
	b.ReportMetric(float64(s.Busy-s0.Busy)/batches/1e6, "ms/tx")
	if s.Failures != s0.Failures {
		b.Errorf("%d commands failed", s.Failures-s0.Failures)
	}
}
