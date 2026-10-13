package loadtest_test

import (
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/loadtest"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
)

// BenchmarkSharedBoard is kanban's 200-tab run in process: 200 streams on
// one board of 200 cards, about 140 KB of HTML, while 200 producers
// change a card 800 times a second between them, so the board changes in
// every tick. All streams use zstd, or all brotli. The database runs with
// synchronous FULL, then NORMAL, checkpointed by the writer. An iteration
// is one run, 2s of warmup and 10s measured; the metrics are the last
// run's.
//
//	taskset -c 0-11 go test ./loadtest -run '^$' -bench SharedBoard -count 3
func BenchmarkSharedBoard(b *testing.B) {
	for _, sync := range []string{"FULL", "NORMAL"} {
		for _, coding := range []string{sse.Zstd, sse.Brotli} {
			b.Run(sync+"/"+coding, func(b *testing.B) { sharedBoard(b, sync, coding, sse.Options{}) })
		}
	}
}

// BenchmarkBoardZstdLevels is BenchmarkSharedBoard with every stream on
// zstd at each level sse.Options.ZstdLevel offers short of the best: the
// bytes per frame against the CPU and the heap per stream.
//
//	taskset -c 0-11 go test ./loadtest -run '^$' -bench BoardZstdLevels -count 3
func BenchmarkBoardZstdLevels(b *testing.B) {
	for _, level := range []zstd.EncoderLevel{zstd.SpeedFastest, zstd.SpeedDefault, zstd.SpeedBetterCompression} {
		b.Run(level.String(), func(b *testing.B) { sharedBoard(b, "FULL", sse.Zstd, sse.Options{ZstdLevel: level}) })
	}
}

func sharedBoard(b *testing.B, sync, coding string, streams sse.Options) {
	board := loadtest.Board{Rows: 200}
	var r loadtest.Report
	for b.Loop() {
		var err error
		r, err = loadtest.Run(b.Context(), loadtest.Config{
			SQLite:         sqlite.Options{Synchronous: sync, Migrations: board.Migrations(), Checkpoint: sqlite.CheckpointWriter},
			Seed:           board.Seed(),
			Streams:        200,
			Stream:         board.Stream,
			AcceptEncoding: []string{coding},
			Producers:      200,
			Rate:           800,
			Command:        board.Command,
			Warmup:         2 * time.Second,
			Duration:       10 * time.Second,
			Hub:            live.Options{SSE: streams},
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
}
