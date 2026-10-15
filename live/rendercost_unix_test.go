//go:build unix

package live

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sse"
)

// TestRenderCost measures what streams that do not take their frames cost
// the hub: 50 tabs, each with a view of its own of about 34 KB that
// changes at every tick, on streams that keep up, on slow ones
// (Spec.Slow), and on streams whose client stopped reading. It reports
// renders and frames a second per stream and the process's CPU, and only
// reports.
//
//	LITH_RENDER_COST=1 go test ./live -run TestRenderCost -v
func TestRenderCost(t *testing.T) {
	if os.Getenv("LITH_RENDER_COST") == "" {
		t.Skip("set LITH_RENDER_COST=1 to measure")
	}
	const tabs = 50
	for _, kind := range []string{"keeps up", "slow", "stalled"} {
		t.Run(kind, func(t *testing.T) { renderCost(t, kind, tabs) })
	}
}

// renderCost measures one kind of TestRenderCost's streams.
func renderCost(t *testing.T, kind string, tabs int) {
	h := start(t, Options{SSE: sse.Options{WriteTimeout: time.Minute}})
	var sinks []*sink
	for i := range tabs {
		key := fmt.Sprintf("tab%d", i)
		var n atomic.Int64
		v := View{Key: key, Every: time.Millisecond, Render: func(context.Context, *sql.Tx) (any, error) {
			k := n.Add(1)
			b := fmt.Appendf(nil, "event: datastar-patch-elements\ndata: elements <ul id=%q>", key)
			for r := range 500 {
				b = fmt.Appendf(b, `<li id="r%d"><span>%d</span><span>row %d of tab %s</span></li>`, r, k, r, key)
			}
			return append(b, "</ul>\n\n"...), nil
		}}
		s := &sink{header: http.Header{}}
		sinks = append(sinks, s)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		r := httptest.NewRequestWithContext(ctx, "GET", "/live", nil)
		go h.Serve(s, r, Spec{ID: key, Views: []View{v}, Slow: kind == "slow"})
	}
	waitFor(t, "the first frames", func() bool { return h.Stats().FramesSent >= int64(tabs) })
	if kind == "stalled" {
		for _, s := range sinks {
			s.stall()
		}
	}
	time.Sleep(time.Second)
	s0, cpu0, t0 := h.Stats(), cpuTime(), time.Now()
	time.Sleep(10 * time.Second)
	s1, cpu1, d := h.Stats(), cpuTime(), time.Since(t0).Seconds()
	per := func(a, b int64) float64 { return float64(b-a) / d / float64(tabs) }
	t.Logf("%s: %.1f renders, %.1f frames sent, %.1f dropped, %.1f deferred a second per stream; %.3f cores busy",
		kind, per(s0.Renders, s1.Renders), per(s0.FramesSent, s1.FramesSent), per(s0.FramesDropped, s1.FramesDropped),
		per(s0.RendersDeferred, s1.RendersDeferred), (cpu1-cpu0).Seconds()/d)
}

// sink is a client that throws its bytes away, or once stalled takes none
// until its write deadline.
type sink struct {
	header   http.Header
	mu       sync.Mutex
	stalled  bool
	deadline time.Time
}

func (s *sink) Header() http.Header { return s.header }
func (s *sink) WriteHeader(int)     {}
func (s *sink) FlushError() error   { return nil }

func (s *sink) Write(p []byte) (int, error) {
	for {
		s.mu.Lock()
		stalled, d := s.stalled, s.deadline
		s.mu.Unlock()
		if !stalled {
			return len(p), nil
		}
		if !d.IsZero() && !time.Now().Before(d) {
			return 0, os.ErrDeadlineExceeded
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *sink) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadline = t
	return nil
}

func (s *sink) stall() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stalled = true
}

// cpuTime is the CPU time the process has used, user and system.
func cpuTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
