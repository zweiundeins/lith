package events_test

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"
)

// BenchmarkRebuildReaders rebuilds a projection of 100,000 events (20,000
// cards, each created and renamed four times) while four readers look up
// cards on the read pool's prepared statements, and reports what the
// rebuild costs them. Each batch renames tables, a schema change, so every
// statement a reader has prepared is prepared again at its next use after
// each batch commits: reprepare-us is that cost, measured on its own.
//
//	go test ./events -run '^$' -bench RebuildReaders -benchtime 1x
func BenchmarkRebuildReaders(b *testing.B) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(b.TempDir(), "rebuild.db"), sqlite.Options{
		Migrations: fstest.MapFS{"001_cards.sql": {Data: []byte(schema)}},
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
	const cards = 20_000
	for first := int64(1); first <= cards; first += 1000 {
		err := w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
			for id := first; id < first+1000; id++ {
				if err := events.Execute(st, tx, create, Cards.ID(id), events.NoStream, Create{Title: "card"}, events.Meta{}); err != nil {
					return err
				}
				for i := range 4 {
					if err := events.Execute(st, tx, rename, Cards.ID(id), events.Any, Rename{Title: fmt.Sprint("title ", i)}, events.Meta{}); err != nil {
						return err
					}
				}
			}
			return nil
		}))
		if err != nil {
			b.Fatal(err)
		}
	}

	// A reader's lookup, alone and after a schema change.
	conn, err := db.R.Conn(ctx)
	if err != nil {
		b.Fatal(err)
	}
	lookup := func(id int64) time.Duration {
		start := time.Now()
		var title string
		if err := conn.QueryRowContext(ctx, `SELECT title FROM card_titles WHERE id = ?`, id).Scan(&title); err != nil {
			b.Fatal(err)
		}
		return time.Since(start)
	}
	var steady, after time.Duration
	for i := range 2000 {
		steady += lookup(int64(1 + i%cards))
	}
	const bumps = 200
	for i := range bumps {
		err := w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
			_, err := tx.ExecUncached(`CREATE TABLE bench_bump (a); DROP TABLE bench_bump`)
			return err
		}))
		if err != nil {
			b.Fatal(err)
		}
		after += lookup(int64(1 + i))
	}
	conn.Close()
	reprepare := after/bumps - steady/2000

	read := func(d time.Duration) (n int64, mean time.Duration) {
		var total atomic.Int64
		var count atomic.Int64
		var wg sync.WaitGroup
		stop := time.Now().Add(d)
		for r := range 4 {
			wg.Go(func() {
				for i := int64(r); time.Now().Before(stop); i += 4 {
					start := time.Now()
					var title string
					if err := db.R.QueryRowContext(ctx, `SELECT title FROM card_titles WHERE id = ?`, 1+i%cards).Scan(&title); err != nil {
						b.Error(err)
						return
					}
					total.Add(int64(time.Since(start)))
					count.Add(1)
				}
			})
		}
		wg.Wait()
		return count.Load(), time.Duration(total.Load() / max(count.Load(), 1))
	}

	for b.Loop() {
		idleN, idleMean := read(2 * time.Second)
		v2 := events.Must(events.New(titles(2, strings.ToUpper)))
		r := events.NewRunner(v2, db, w, events.Options{Log: slog.New(slog.DiscardHandler)})
		runCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		start := time.Now()
		go func() { done <- r.Run(runCtx) }()
		var took time.Duration
		var duringN int64
		var duringMean time.Duration
		var mu sync.Mutex
		finished, readersDone := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(readersDone)
			for {
				n, mean := read(100 * time.Millisecond)
				mu.Lock()
				if duringN == 0 {
					duringMean = mean
				} else {
					duringMean = time.Duration((int64(duringMean)*duringN + int64(mean)*n) / (duringN + n))
				}
				duringN += n
				mu.Unlock()
				select {
				case <-finished:
					return
				default:
				}
			}
		}()
		for {
			if s := r.Stats(); len(s.Projectors) == 1 && s.Projectors[0].Status == "live" {
				took = time.Since(start)
				break
			}
			time.Sleep(time.Millisecond)
		}
		close(finished)
		<-readersDone
		stop()
		if err := <-done; err != nil {
			b.Fatal(err)
		}
		mu.Lock()
		b.ReportMetric(took.Seconds(), "rebuild-s")
		b.ReportMetric(float64(idleN)/2, "reads/s-idle")
		b.ReportMetric(float64(duringN)/took.Seconds(), "reads/s-rebuild")
		b.ReportMetric(float64(idleMean.Nanoseconds())/1e3, "read-us-idle")
		b.ReportMetric(float64(duringMean.Nanoseconds())/1e3, "read-us-rebuild")
		b.ReportMetric(float64(reprepare.Nanoseconds())/1e3, "reprepare-us")
		mu.Unlock()
		var n int
		if err := db.R.QueryRow(`SELECT count(*) FROM card_titles WHERE title = upper(title)`).Scan(&n); err != nil || n != cards {
			b.Fatalf("%d cards rebuilt, %v", n, err)
		}
	}
	b.ReportMetric(0, "ns/op")
}
