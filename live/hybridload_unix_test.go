//go:build unix

package live

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sse"
)

// TestHybridLoad is a rough measure of what pacing costs the server with
// many viewers of one view, without browsers: a Game of Life of 80 by 60
// cells in rows of short tags, as on lith's site, that steps ten times a
// second, watched by N zstd streams whose pages take every write at once.
// A fraction of the pages is slow: it acknowledges a write 400 ms after
// it goes out, a fast page after 20 ms. Three ways to serve them:
// "plugin" sends every stream every frame (the page drops what it cannot
// apply, newest-frame.js); "ack" paces every stream from the start
// (Spec.Window 1); "hybrid" paces only the slow pages, 3 s after they
// open, as newest-frame-behind would. Acknowledgements call Hub.Ack
// directly, or with LIVE_HYBRID_LOAD_HTTP=1 go through a POST to an HTTP
// server in the same process, whose client side then counts too.
// LIVE_HYBRID_LOAD_ROUNDS runs the three in turn that many times.
//
//	LIVE_HYBRID_LOAD=200,1000 go test ./live -run TestHybridLoad -v -timeout 30m
func TestHybridLoad(t *testing.T) {
	ns := os.Getenv("LIVE_HYBRID_LOAD")
	if ns == "" {
		t.Skip("set LIVE_HYBRID_LOAD to the numbers of streams")
	}
	slowShare := 0.1
	if f, err := strconv.ParseFloat(os.Getenv("LIVE_HYBRID_LOAD_SLOW"), 64); err == nil {
		slowShare = f
	}
	viaHTTP := os.Getenv("LIVE_HYBRID_LOAD_HTTP") == "1"
	rounds, _ := strconv.Atoi(os.Getenv("LIVE_HYBRID_LOAD_ROUNDS"))
	rounds = max(rounds, 1)
	t.Logf("GOMAXPROCS %d, slow share %.2f, acknowledgements over HTTP %v", runtime.GOMAXPROCS(0), slowShare, viaHTTP)
	t.Logf("| Mode | Streams | CPU (%% of a core) | Acks/s | Wire KB/s | Frames sent/s | Frames dropped/s | Renders/s | Renders deferred/s |")
	for _, f := range strings.Split(ns, ",") {
		n, _ := strconv.Atoi(f)
		for _, mode := range slices.Repeat([]string{"plugin", "ack", "hybrid"}, rounds) {
			r := hybridLoad(t, mode, n, slowShare, viaHTTP)
			t.Logf("| %s | %d | %.1f | %.0f | %.0f | %.0f | %.0f | %.1f | %.1f |", mode, n, r.cpu, r.acks, r.wire/1000, r.sent, r.dropped, r.renders, r.deferred)
		}
	}
}

type loadResult struct {
	cpu, acks, wire, sent, dropped, renders, deferred float64
}

func hybridLoad(t *testing.T, mode string, n int, slowShare float64, viaHTTP bool) loadResult {
	const warmup, span = 5 * time.Second, 20 * time.Second
	h := New(Options{Tick: 20 * time.Millisecond, MaxStreams: n + 10, Log: slog.New(slog.DiscardHandler)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	ack := h.Ack
	if viaHTTP {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			k, _ := strconv.ParseInt(q.Get("n"), 10, 64)
			h.Ack(q.Get("tab"), q.Get("s"), k)
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()
		cl := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: n + 10}}
		ack = func(id, token string, k int64) bool {
			res, err := cl.Post(srv.URL+"/ack?tab="+id+"&s="+token+"&n="+strconv.FormatInt(k, 10), "", nil)
			if err == nil {
				res.Body.Close()
			}
			return err == nil
		}
	}

	board := lifeView()
	var streams []*sinkStream
	for i := range n {
		slow := float64(i) < slowShare*float64(n)
		delay := 20 * time.Millisecond
		if slow {
			delay = 400 * time.Millisecond
		}
		id := "tab" + strconv.Itoa(i)
		spec := Spec{ID: id, Views: []View{board}}
		if mode != "plugin" {
			spec.Ack = func(token string, k int64) []byte {
				time.AfterFunc(delay, func() { ack(id, token, k) })
				return sse.PatchElements([]byte(`<i id="ack-` + strconv.FormatInt(k, 10) + `"></i>`))
			}
		}
		if mode == "ack" {
			spec.Window = 1
		}
		s := openSink(h, spec)
		streams = append(streams, s)
		if mode == "hybrid" && slow {
			time.AfterFunc(3*time.Second, func() { h.Pace(id, 1) })
		}
	}
	defer func() {
		for _, s := range streams {
			s.cancel()
		}
		for _, s := range streams {
			<-s.done
		}
	}()

	time.Sleep(warmup)
	wire := func() (b int64) {
		for _, s := range streams {
			b += s.bytes.Load()
		}
		return b
	}
	s0, w0, c0, t0 := h.Stats(), wire(), processCPU(), time.Now()
	time.Sleep(span)
	s1, w1, c1 := h.Stats(), wire(), processCPU()
	secs := time.Since(t0).Seconds()
	return loadResult{
		cpu:      float64(c1-c0) / 1e9 / secs * 100,
		acks:     float64(s1.Acks-s0.Acks) / secs,
		wire:     float64(w1-w0) / secs,
		sent:     float64(s1.FramesSent-s0.FramesSent) / secs,
		dropped:  float64(s1.FramesDropped-s0.FramesDropped) / secs,
		renders:  float64(s1.Renders-s0.Renders) / secs,
		deferred: float64(s1.RendersDeferred-s0.RendersDeferred) / secs,
	}
}

// lifeView is a Game of Life that steps once per render, at most ten
// times a second, with a fresh soup every 15 s, in rows of cells as on
// lith's site.
func lifeView() View {
	const w, hgt = 80, 60
	var mu sync.Mutex
	cells := make([]bool, w*hgt)
	next := make([]bool, w*hgt)
	rng := rand.New(rand.NewPCG(1, 2))
	soup := func() {
		for i := range cells {
			cells[i] = rng.IntN(3) == 0
		}
	}
	soup()
	gen := 0
	return View{Key: "life", Every: 100 * time.Millisecond, Render: func(context.Context, *sql.Tx) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		if gen++; gen%150 == 0 {
			soup()
		}
		for y := range hgt {
			for x := range w {
				k := 0
				for dy := -1; dy <= 1; dy++ {
					for dx := -1; dx <= 1; dx++ {
						if (dx != 0 || dy != 0) && cells[(y+dy+hgt)%hgt*w+(x+dx+w)%w] {
							k++
						}
					}
				}
				next[y*w+x] = k == 3 || k == 2 && cells[y*w+x]
			}
		}
		cells, next = next, cells
		var b strings.Builder
		b.WriteString(`<div id="life-board" class="life__board" aria-hidden="true">`)
		for y := range hgt {
			fmt.Fprintf(&b, `<div id="life-r%d">`, y)
			for _, c := range cells[y*w : (y+1)*w] {
				if c {
					b.WriteString(`<i class="c1"></i>`)
				} else {
					b.WriteString(`<i></i>`)
				}
			}
			b.WriteString(`</div>`)
		}
		b.WriteString(`</div>`)
		return sse.PatchElements([]byte(b.String())), nil
	}}
}

// sinkStream is a zstd stream whose page takes every write at once and
// keeps only its size.
type sinkStream struct {
	header http.Header
	cancel context.CancelFunc
	done   chan struct{}
	bytes  atomic.Int64
}

func openSink(h *Hub, spec Spec) *sinkStream {
	ctx, cancel := context.WithCancel(context.Background())
	s := &sinkStream{header: http.Header{}, cancel: cancel, done: make(chan struct{})}
	r := httptest.NewRequestWithContext(ctx, "GET", "/live", nil)
	r.Header.Set("Accept-Encoding", "zstd")
	go func() {
		defer close(s.done)
		h.Serve(s, r, spec)
	}()
	return s
}

func (s *sinkStream) Header() http.Header              { return s.header }
func (s *sinkStream) WriteHeader(int)                  {}
func (s *sinkStream) FlushError() error                { return nil }
func (s *sinkStream) SetWriteDeadline(time.Time) error { return nil }
func (s *sinkStream) Write(p []byte) (int, error)      { s.bytes.Add(int64(len(p))); return len(p), nil }
func processCPU() int64 {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return ru.Utime.Nano() + ru.Stime.Nano()
}
