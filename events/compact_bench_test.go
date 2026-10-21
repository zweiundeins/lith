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

// BenchmarkCompact compacts one stream, a command at a time, as a sweep
// in each command does. compact-us is Compact's own time, without the
// commit.
//
// window is a stream that keeps 100,000 events in its window of time, as
// a collaborative document does: each command appends an event and
// compacts one with Limit 1, which deletes the stream's snapshot and
// rewrites the next event as the new one.
//
// backlog is a stream of 90,000 events, 40,000 of them older than Since
// and not compacted yet: each command appends an event and compacts 10,
// while Since moves on by an event.
//
//	go test ./events -run '^$' -bench Compact -benchtime 2000x
func BenchmarkCompact(b *testing.B) {
	b.Run("window", func(b *testing.B) {
		const window, settled = 100_000, 10
		w, st, t0 := compactBench(b, window+settled)
		err := w.Do(context.Background(), writer.Func(func(tx *writer.Tx) error {
			_, err := events.Compact(st, tx, AccountBalanced, events.Retention{Since: time.UnixMilli(t0 + settled)}, move, balancedOf)
			return err
		}))
		if err != nil {
			b.Fatal(err)
		}
		runCompact(b, w, st, t0, window+settled, func(k int) events.Retention {
			return events.Retention{Since: time.UnixMilli(t0 + int64(settled+1+k)), Limit: 1}
		}, 1)
	})
	b.Run("backlog", func(b *testing.B) {
		const stream, old = 90_000, 40_000
		w, st, t0 := compactBench(b, stream)
		runCompact(b, w, st, t0, stream, func(k int) events.Retention {
			return events.Retention{Since: time.UnixMilli(t0 + int64(old+k)), Limit: 10}
		}, 9)
	})
}

// compactBench runs a writer on a new database and appends n events to
// account 1, event i stored at t0 + i ms.
func compactBench(b *testing.B, n int) (*writer.Writer, *events.Store, int64) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(b.TempDir(), "compact.db"), sqlite.Options{
		Synchronous: "NORMAL",
		Migrations:  fstest.MapFS{"001_cards.sql": {Data: []byte(schema)}},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	w, err := writer.New(db, writer.Options{Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		b.Fatal(err)
	}
	go w.Run(ctx)
	b.Cleanup(func() { w.Close() })
	st := events.Must(events.New())
	t0 := time.Now().Add(-time.Hour).UnixMilli()
	for first := 0; first < n; first += 10_000 {
		err := w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
			for i := first; i < min(first+10_000, n); i++ {
				if err := events.Append(st, tx, Accounts.ID(1), events.Any, events.Meta{At: time.UnixMilli(t0 + int64(i))}, AccountEvent(Deposited{Amount: 1})); err != nil {
					return err
				}
			}
			return nil
		}))
		if err != nil {
			b.Fatal(err)
		}
	}
	return w, st, t0
}

// runCompact runs b.N commands, each of which appends event next+k and
// compacts with keep(k) no fewer than least events.
func runCompact(b *testing.B, w *writer.Writer, st *events.Store, t0 int64, next int, keep func(int) events.Retention, least int64) {
	ctx := context.Background()
	var spent time.Duration
	b.ResetTimer()
	for k := range b.N {
		err := w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
			if err := events.Append(st, tx, Accounts.ID(1), events.Any, events.Meta{At: time.UnixMilli(t0 + int64(next+k))}, AccountEvent(Deposited{Amount: 1})); err != nil {
				return err
			}
			start := time.Now()
			n, err := events.Compact(st, tx, AccountBalanced, keep(k), move, balancedOf)
			spent += time.Since(start)
			if err == nil && n < least {
				b.Errorf("compacted %d events, want %d or more", n, least)
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
