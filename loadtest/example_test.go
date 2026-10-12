package loadtest_test

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"testing/fstest"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/loadtest"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"
)

// Ten tabs on one board of 50 cards, which two producers change a
// hundred times a second. The numbers differ from run to run; fmt.Println(r)
// lays them all out.
func Example() {
	b := loadtest.Board{Rows: 50}
	r, err := loadtest.Run(context.Background(), loadtest.Config{
		SQLite:    sqlite.Options{Migrations: b.Migrations()},
		Seed:      b.Seed(),
		Streams:   10,
		Stream:    b.Stream,
		Producers: 2,
		Rate:      100,
		Command:   b.Command,
		Duration:  500 * time.Millisecond,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(r.Wire[sse.Zstd].Streams, "zstd streams")
	fmt.Println(r.Errors, "failed commands")
	fmt.Println(r.Ticks > 0 && r.FramesSent > 0)
	// Output:
	// 10 zstd streams
	// 0 failed commands
	// true
}

// A project measures its own views and commands: here a count on every
// page and a command that adds to it, with half the streams on brotli.
func ExampleRun() {
	migrations := fstest.MapFS{"001_count.sql": {Data: []byte(`
		CREATE TABLE count (id INTEGER PRIMARY KEY, n INTEGER NOT NULL);
		INSERT INTO count VALUES (1, 0);`)}}
	count := live.View{
		Key:    "count",
		Topics: []string{"count"},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT n FROM count WHERE id = 1`).Scan(&n); err != nil {
				return nil, err
			}
			return sse.PatchElements(fmt.Appendf(nil, `<p id="count">%d</p>`, n)), nil
		},
	}
	add := writer.Func(func(tx *writer.Tx) error {
		_, err := tx.Exec(`UPDATE count SET n = n + 1 WHERE id = 1`)
		tx.Touch("count")
		return err
	})

	r, err := loadtest.Run(context.Background(), loadtest.Config{
		SQLite:         sqlite.Options{Synchronous: "NORMAL", Migrations: migrations},
		Streams:        20,
		Stream:         func(int) live.Spec { return live.Spec{Views: []live.View{count}} },
		AcceptEncoding: []string{"gzip, br, zstd", "gzip, br"},
		Producers:      4,
		Rate:           200,
		Command:        func(int, int64) writer.Command { return add },
		Warmup:         100 * time.Millisecond,
		Duration:       500 * time.Millisecond,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(r.Wire[sse.Zstd].Streams, "zstd and", r.Wire[sse.Brotli].Streams, "brotli streams")
	fmt.Println(math.Round(r.RendersPerTick()), "render per tick")
	// Output:
	// 10 zstd and 10 brotli streams
	// 1 render per tick
}
