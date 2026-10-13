package cursorparty

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/loadtest"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/migrate"
	"github.com/zweiundeins/lith/site/migrations"
)

// BenchmarkParty is the party at its busiest: 50 people whose pointers
// never rest, each sending the page's full 15 moves a second, 750 between
// them, while tabs watch the layer and the counter, every stream on zstd,
// as browsers ask. It runs as the site does: two render workers,
// GOMEMLIMIT 192 MiB and the streams' budget at half of it, hibernation
// after a minute. 50 tabs are the movers alone; 300 are the site's cap
// (-streams 300), where the budget leaves streams uncompressed. An
// iteration is one run, 2 s of warmup and 10 s measured.
//
//	go test ./internal/demos/cursor-party -run '^$' -bench Party -benchtime 1x
func BenchmarkParty(b *testing.B) {
	for _, tabs := range []int{50, 300} {
		b.Run("tabs="+strconv.Itoa(tabs), func(b *testing.B) { benchParty(b, tabs) })
	}
}

func benchParty(b *testing.B, tabs int) {
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(192 << 20))
	const movers = 50
	var (
		r       loadtest.Report
		budget  sse.BudgetStats
		layer   frameSize
		rssPeak int64
	)
	for b.Loop() {
		db := openDB(b)
		bud := sse.NewBudget(sse.BudgetOptions{}) // half of GOMEMLIMIT: 96 MiB
		stop := watch(func() {
			s := bud.Stats()
			if s.Reserved >= budget.Reserved {
				budget = s
			}
			rssPeak = max(rssPeak, rss())
		})
		var err error
		r, err = loadtest.Run(b.Context(), loadtest.Config{
			DB:             db,
			Streams:        tabs,
			Stream:         func(i int) live.Spec { return spec(i, &layer) },
			AcceptEncoding: []string{"gzip, deflate, br, zstd"},
			Producers:      movers,
			Rate:           movers / Throttle.Seconds(),
			Command:        moves,
			Warmup:         2 * time.Second,
			Duration:       10 * time.Second,
			Hub:            live.Options{Workers: 2, Hibernate: time.Minute, SSE: sse.Options{Budget: bud}},
		})
		stop()
		db.Close()
		if err != nil {
			b.Fatal(err)
		}
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	b.Logf("\n%s\nlayer: %s\nbudget: %d MiB of %d MiB reserved at most, streams %v\nprocess: %.0f MiB resident at most, heap in use %.0f MiB after the run",
		r, layer.String(), budget.Reserved>>20, budget.Limit>>20, budget.Streams, float64(rssPeak)/(1<<20), float64(m.HeapInuse)/(1<<20))
	for _, x := range r.Metrics() {
		b.ReportMetric(x.Value, x.Unit)
	}
	b.ReportMetric(layer.mean(), "layer-B/frame")
	b.ReportMetric(float64(rssPeak)/(1<<20), "rss-MiB")
	b.ReportMetric(0, "ns/op")
}

// spec is tab i's stream, as the site writes it: the frames of a tick in
// one write. It measures the layer's frames as they leave the render, before
// compression.
func spec(i int, layer *frameSize) live.Spec {
	v := demo.Viewer{Session: "watcher", Tab: "tab-" + strconv.Itoa(i)}
	var events [][]byte
	return live.Spec{ID: v.Tab, Views: views(v), WriteTick: func(st *sse.Stream, frames []live.Frame) error {
		events = events[:0]
		for _, f := range frames {
			b := f.Value.([]byte)
			if i == 0 && f.Key == cursorsKey {
				layer.add(len(b))
			}
			events = append(events, b)
		}
		return st.Write(events...)
	}}
}

// moves is producer p's i-th move: its pointer circles the floor, each
// producer on a circle of its own.
func moves(p int, i int64) writer.Command {
	a := float64(i)/15 + float64(p)
	x := 0.5 + 0.4*math.Cos(a)*math.Sin(float64(p))
	y := 0.5 + 0.4*math.Sin(a*0.7+float64(p))
	return Move{Viewer: demo.Viewer{Session: "mover-" + strconv.Itoa(p), Tab: "mover-" + strconv.Itoa(p)}, X: x, Y: y, Seq: i + 1, At: time.Now()}
}

func openDB(b *testing.B) *sqlite.DB {
	db, err := sqlite.Open(context.Background(), filepath.Join(b.TempDir(), "site.db"), sqlite.Options{
		Migrate: func(ctx context.Context, w *sql.DB) error {
			return migrate.Run(ctx, w, []migrate.Scope{{Name: "site", FS: migrations.FS}, {Name: Slug, FS: Demo.Migrations}})
		},
		Readers:    2 + 4, // as the site: its render workers and four more
		CacheKiB:   4096,
		Checkpoint: sqlite.CheckpointWriter,
	})
	if err != nil {
		b.Fatal(err)
	}
	return db
}

// frameSize counts the layer's frames and their bytes.
type frameSize struct {
	mu        sync.Mutex
	n, sum, m int
}

func (f *frameSize) add(size int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	f.sum += size
	f.m = max(f.m, size)
}

func (f *frameSize) mean() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n == 0 {
		return 0
	}
	return float64(f.sum) / float64(f.n)
}

func (f *frameSize) String() string {
	return fmt.Sprintf("%.0f bytes a frame before compression, %d at most", f.mean(), f.m)
}

// watch calls fn every 100 ms until stop.
func watch(fn func()) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				fn()
			}
		}
	})
	return func() { close(done); wg.Wait() }
}

// rss is the process's resident memory, in bytes (Linux; 0 elsewhere).
func rss() int64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			kb, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
			return kb << 10
		}
	}
	return 0
}
