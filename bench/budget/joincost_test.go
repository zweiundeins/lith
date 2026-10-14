package budget

import (
	"context"
	"encoding/csv"
	"hash/maphash"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"
)

// joinCost is what streams that join a view cost, per join.
type joinCost struct {
	frame   int     // bytes of the view's frame
	renders float64 // renders of the view per join
	cpu     time.Duration
	alloc   float64 // bytes allocated per join
	kept    int64   // bytes the budget books for the view's frame meanwhile
}

// measureJoins opens watchers streams on a table view of rows rows, then
// joins streams to it one after another, each until it has its first
// frame, and measures the renders, the CPU and the allocations per join.
func measureJoins(t *testing.T, rows, watchers, joins int) joinCost {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b := sse.NewBudget(sse.BudgetOptions{Limit: -1})
	h := live.New(live.Options{SSE: sse.Options{Budget: b}, Tick: 2 * time.Millisecond, Heartbeat: time.Hour})
	ran := make(chan struct{})
	go func() { h.Run(ctx); close(ran) }()
	var version atomic.Int64
	v := tableView("table", rows, &version)
	open := func() (*countingSink, chan struct{}, context.CancelFunc) {
		s := &countingSink{header: http.Header{}}
		sctx, stop := context.WithCancel(ctx)
		r := httptest.NewRequestWithContext(sctx, "GET", "/", nil)
		r.Header.Set("Accept-Encoding", "identity")
		done := make(chan struct{})
		go func() { h.Serve(s, r, live.Spec{Views: []live.View{v}}); close(done) }()
		// Its headers, then its first frame.
		for deadline := time.Now().Add(time.Minute); s.flushes.Load() < 2; time.Sleep(50 * time.Microsecond) {
			if time.Now().After(deadline) {
				t.Fatal("a stream got no frame in a minute")
			}
		}
		return s, done, stop
	}
	for range watchers {
		open()
	}
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	renders, cpu0 := h.Stats().Renders, cpuTime()
	for range joins {
		_, done, stop := open()
		stop()
		<-done
	}
	cpu := cpuTime() - cpu0
	runtime.ReadMemStats(&m1)
	f, _ := v.Render(ctx, nil)
	c := joinCost{
		frame:   len(f.([]byte)),
		renders: float64(h.Stats().Renders-renders) / float64(joins),
		cpu:     cpu / time.Duration(joins),
		alloc:   float64(m1.TotalAlloc-m0.TotalAlloc) / float64(joins),
		kept:    b.Stats().Frames,
	}
	cancel()
	<-ran
	return c
}

// hashTime is how long the hub takes to hash a frame of the table for the
// unchanged-frame check: the mean of many hashes.
func hashTime(t *testing.T, rows int) time.Duration {
	var version atomic.Int64
	f, err := tableView("table", rows, &version).Render(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, seed := f.([]byte), maphash.MakeSeed()
	const n = 2000
	var sum uint64
	start := time.Now()
	for range n {
		sum += maphash.Bytes(seed, b)
	}
	d := time.Since(start) / n
	runtime.KeepAlive(sum)
	return d
}

// TestJoinCost measures what a stream that joins a view costs when the hub
// keeps the view's frame (two streams watch it) and when it keeps none
// (one stream watches it, and the join renders it), for data tables of
// 0.8, 49, 143 and 233 KB: the renders, the CPU and the bytes allocated
// per join, and what the budget books for the frame; and how long the hub
// takes to hash the frame. It writes joincost.csv. LITH_BUDGET_RUNS
// repeats the runs; the CPU is then the median.
func TestJoinCost(t *testing.T) {
	request(t)
	const joins = 200
	f, err := os.Create("joincost.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := csv.NewWriter(f)
	c.Write([]string{"rows", "frame_bytes", "watchers", "frame", "joins", "renders_per_join", "cpu_us_per_join", "alloc_kib_per_join", "frame_kib_booked", "hash_us"})
	for _, rows := range []int{2, 150, 430, 700} {
		hash := hashTime(t, rows)
		for _, watchers := range []int{1, 2} {
			var r joinCost
			var cpus []time.Duration
			for range runs() {
				r = measureJoins(t, rows, watchers, joins)
				cpus = append(cpus, r.cpu)
			}
			cpu, lo, hi := median(cpus)
			frame := "kept"
			if watchers == 1 {
				frame = "dropped"
			}
			c.Write([]string{strconv.Itoa(rows), strconv.Itoa(r.frame), strconv.Itoa(watchers), frame, strconv.Itoa(joins),
				strconv.FormatFloat(r.renders, 'f', 2, 64), strconv.FormatFloat(float64(cpu)/1e3, 'f', 0, 64),
				strconv.FormatFloat(r.alloc/1024, 'f', 0, 64), strconv.FormatFloat(float64(r.kept)/1024, 'f', 0, 64),
				strconv.FormatFloat(float64(hash)/1e3, 'f', 1, 64)})
			c.Flush()
			t.Logf("%4d rows (%6d B), %d watching, frame %-7s: %.2f renders, %5.0f µs CPU (%.0f to %.0f), %5.0f KiB allocated per join; %4.0f KiB booked; %.1f µs a hash",
				rows, r.frame, watchers, frame, r.renders, float64(cpu)/1e3, float64(lo)/1e3, float64(hi)/1e3, r.alloc/1024, float64(r.kept)/1024, float64(hash)/1e3)
		}
	}
	if err := c.Error(); err != nil {
		t.Fatal(err)
	}
}
