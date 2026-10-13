package loadtest_test

import (
	"testing"
	"time"

	"github.com/zweiundeins/lith/loadtest"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
)

// BenchmarkSharedBoard is kanban's 200-tab run in process: 200 streams on
// one board of 200 cards, about 140 KB of HTML, while 200 producers
// change a card 800 times a second between them, so the board changes in
// every tick. All streams use zstd, or all brotli. The database runs as
// the projects run it: FULL, checkpointed by the writer. An iteration is
// one run, 2s of warmup and 10s measured; the metrics are the last run's.
//
//	taskset -c 0-11 go test ./loadtest -run '^$' -bench SharedBoard -count 3
func BenchmarkSharedBoard(b *testing.B) {
	board := loadtest.Board{Rows: 200}
	for _, coding := range []string{sse.Zstd, sse.Brotli} {
		b.Run(coding, func(b *testing.B) {
			var r loadtest.Report
			for b.Loop() {
				var err error
				r, err = loadtest.Run(b.Context(), loadtest.Config{
					SQLite:         sqlite.Options{Migrations: board.Migrations(), Checkpoint: sqlite.CheckpointWriter},
					Seed:           board.Seed(),
					Streams:        200,
					Stream:         board.Stream,
					AcceptEncoding: []string{coding},
					Producers:      200,
					Rate:           800,
					Command:        board.Command,
					Warmup:         2 * time.Second,
					Duration:       10 * time.Second,
				})
				if err != nil {
					b.Fatal(err)
				}
			}
			b.Log("\n" + r.String())
			for _, m := range r.Metrics() {
				b.ReportMetric(m.Value, m.Unit)
			}
			b.ReportMetric(0, "ns/op")
		})
	}
}
