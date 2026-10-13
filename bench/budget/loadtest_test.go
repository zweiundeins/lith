package budget

import (
	"encoding/csv"
	"fmt"
	"maps"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/loadtest"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
)

// TestLoadtest runs BenchmarkSharedBoard's board in process with 200 and
// 1,000 streams, while 200 producers change a card 800 times a second:
// with lith's default encoder, with a static low-memory window at the
// board's floor (2^18), and with a budget of 128 MiB at that floor. It
// writes loadtest.csv. LITH_BUDGET_RUNS repeats each run.
//
//	LITH_BUDGET=1 taskset -c 0-11 go test ./bench/budget -run Loadtest -v -timeout 1h
func TestLoadtest(t *testing.T) {
	request(t)
	board := loadtest.Board{Rows: 200}
	f, err := os.Create("loadtest.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := csv.NewWriter(f)
	c.Write([]string{"streams", "policy", "run", "cores", "ticks_per_s", "tick_p99_ms", "frames_per_s", "frames_dropped", "bytes_per_frame",
		"heap_mib", "heap_mib_per_stream", "reserved_mib", "tiers", "load_before", "load_after"})
	for _, streams := range []int{200, 1000} {
		for _, policy := range []string{"lith default", "static low memory", "budget"} {
			for run := range runs() {
				var b *sse.Budget
				var opt sse.Options
				switch policy {
				case "static low memory":
					opt = sse.Options{ZstdWindow: 18, ZstdLowMemory: true}
				case "budget":
					b = sse.NewBudget(sse.BudgetOptions{Limit: 128 << 20, MinWindow: 18, MaxWindow: 19})
					opt.Budget = b
				}
				// The budget's streams by tier, last seen with every stream open.
				var last sse.BudgetStats
				done := make(chan struct{})
				sampled := make(chan struct{})
				go func() {
					defer close(sampled)
					tick := time.NewTicker(200 * time.Millisecond)
					defer tick.Stop()
					for {
						select {
						case <-done:
							return
						case <-tick.C:
						}
						if b == nil {
							continue
						}
						s := b.Stats()
						n := 0
						for _, k := range s.Streams {
							n += k
						}
						if n == streams {
							last = s
						}
					}
				}()
				before := loadavg()
				runtime.GC()
				r, err := loadtest.Run(t.Context(), loadtest.Config{
					SQLite:    sqlite.Options{Migrations: board.Migrations(), Checkpoint: sqlite.CheckpointWriter},
					Seed:      board.Seed(),
					Streams:   streams,
					Stream:    board.Stream,
					Producers: 200,
					Rate:      800,
					Command:   board.Command,
					Warmup:    2 * time.Second,
					Duration:  10 * time.Second,
					Hub:       live.Options{SSE: opt},
				})
				close(done)
				<-sampled
				if err != nil {
					t.Fatal(err)
				}
				after := loadavg()
				w := r.Wire[sse.Zstd]
				var tiers []string
				for _, k := range slices.Sorted(maps.Keys(last.Streams)) {
					tiers = append(tiers, fmt.Sprintf("%s: %d", k, last.Streams[k]))
				}
				heap := float64(r.HeapPerStream) * float64(streams) / mib
				secs := r.Duration.Seconds()
				c.Write([]string{strconv.Itoa(streams), policy, strconv.Itoa(run + 1),
					strconv.FormatFloat(r.CPU, 'f', 2, 64), strconv.FormatFloat(float64(r.Ticks)/secs, 'f', 1, 64),
					strconv.FormatFloat(float64(r.Tick.P99)/1e6, 'f', 2, 64), strconv.FormatFloat(float64(r.FramesSent)/secs, 'f', 0, 64),
					strconv.FormatInt(r.FramesDropped, 10), strconv.FormatFloat(w.PerWrite(), 'f', 0, 64),
					strconv.FormatFloat(heap, 'f', 1, 64), strconv.FormatFloat(float64(r.HeapPerStream)/mib, 'f', 3, 64),
					strconv.FormatFloat(float64(last.Reserved)/mib, 'f', 1, 64), strings.Join(tiers, "; "), before, after})
				c.Flush()
				t.Logf("%4d streams, %-17s run %d: %.2f cores, %.1f ticks/s, tick p99 %v, %.0f frames/s, %d dropped, %.0f B/frame, heap %.1f MiB (%.3f per stream), %s; load %s -> %s",
					streams, policy, run+1, r.CPU, float64(r.Ticks)/secs, r.Tick.P99.Round(10*time.Microsecond), float64(r.FramesSent)/secs, r.FramesDropped,
					w.PerWrite(), heap, float64(r.HeapPerStream)/mib, strings.Join(tiers, "; "), before, after)
			}
		}
	}
}
