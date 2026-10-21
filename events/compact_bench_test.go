package events_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"
)

// BenchmarkCompact is a stream that keeps 100,000 events in its window of
// time, as a collaborative document does: each command appends an event
// and compacts one with Limit 1, which deletes the stream's snapshot and
// rewrites the next event as the new one. compact-us is Compact's own
// time, without the commit.
//
//	go test ./events -run '^$' -bench Compact -benchtime 2000x
func BenchmarkCompact(b *testing.B) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(b.TempDir(), "compact.db"), sqlite.Options{
		Synchronous: "NORMAL",
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
	st := events.Must(events.New())

	// Event i is stored at t0 + i ms, so Since t0 + n ms keeps from event n.
	const window, settled = 100_000, 10
	t0 := time.Now().Add(-time.Hour).UnixMilli()
	at := func(i int) events.Meta { return events.Meta{At: time.UnixMilli(t0 + int64(i))} }
	id := Accounts.ID(1)
	for first := 0; first < window+settled; first += 10_000 {
		err := w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
			for i := first; i < min(first+10_000, window+settled); i++ {
				if err := events.Append(st, tx, id, events.Any, at(i), AccountEvent(Deposited{Amount: 1})); err != nil {
					return err
				}
			}
			return nil
		}))
		if err != nil {
			b.Fatal(err)
		}
	}
	keep := func(n int) events.Retention {
		return events.Retention{Since: time.UnixMilli(t0 + int64(n)), Limit: 1}
	}
	err = w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
		_, err := events.Compact(st, tx, AccountBalanced, events.Retention{Since: time.UnixMilli(t0 + settled)}, move, balancedOf)
		return err
	}))
	if err != nil {
		b.Fatal(err)
	}

	var spent time.Duration
	b.ResetTimer()
	for k := range b.N {
		err := w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
			if err := events.Append(st, tx, id, events.Any, at(window+settled+k), AccountEvent(Deposited{Amount: 1})); err != nil {
				return err
			}
			start := time.Now()
			n, err := events.Compact(st, tx, AccountBalanced, keep(settled+1+k), move, balancedOf)
			spent += time.Since(start)
			if err == nil && n != 1 {
				b.Errorf("compacted %d events, want 1", n)
			}
			return err
		}))
		if err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(spent.Microseconds())/float64(b.N), "compact-us")
}
