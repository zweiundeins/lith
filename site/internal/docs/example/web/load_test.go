package web

import (
	"fmt"
	"testing"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/loadtest"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/docs/example/board"
	"github.com/zweiundeins/lith/site/internal/docs/example/board/migrations"
)

// BenchmarkBoard runs the board's own view and commands under load, in
// process: 200 tabs on board 1, each with its tailored stream, while 200
// producers rename cards 800 times a second between them.
//
//	go test ./internal/docs/example/web -run '^$' -bench Board -count 3
func BenchmarkBoard(b *testing.B) {
	var r loadtest.Report
	for b.Loop() {
		var err error
		if r, err = loadtest.Run(b.Context(), load(200, 800, 2*time.Second, 10*time.Second)); err != nil {
			b.Fatal(err)
		}
	}
	b.Log("\n" + r.String())
	for _, m := range r.Metrics() {
		b.ReportMetric(m.Value, m.Unit)
	}
	b.ReportMetric(0, "ns/op")
}

// TestLoad is BenchmarkBoard small and short, so that the tests keep it
// working.
func TestLoad(t *testing.T) {
	r, err := loadtest.Run(t.Context(), load(10, 100, 100*time.Millisecond, 400*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if r.Errors > 0 || r.Ticks == 0 || r.FramesSent == 0 || r.FramesDropped > 0 {
		t.Errorf("load test:\n%s", r)
	}
}

// load is a run on board 1 with 100 cards in its first lane.
func load(streams int, rate float64, warmup, d time.Duration) loadtest.Config {
	seed := writer.Func(func(tx *writer.Tx) error {
		if _, err := tx.Exec(`INSERT INTO boards (id, name) VALUES (1, 'Load');
			INSERT INTO lanes (id, board_id, name, max_cards) VALUES (1, 1, 'To do', 1000), (2, 1, 'Done', 1000)`); err != nil {
			return err
		}
		for i := range 100 {
			if _, err := tx.Exec(`INSERT INTO cards (lane_id, title) VALUES (1, ?)`, fmt.Sprint("card ", i)); err != nil {
				return err
			}
		}
		return nil
	})
	return loadtest.Config{
		SQLite: sqlite.Options{Migrations: migrations.FS},
		Seed:   seed,
		Stream: func(int) live.Spec {
			return live.Spec{Views: []live.View{boardView(1)}, WriteTick: tailor()}
		},
		Streams:   streams,
		Producers: streams,
		Rate:      rate,
		Command: func(p int, i int64) writer.Command {
			return board.RenameCard{Card: 1 + (int64(p)*7+i)%100, Title: fmt.Sprint("renamed ", i), At: time.Now()}
		},
		Warmup:   warmup,
		Duration: d,
	}
}
