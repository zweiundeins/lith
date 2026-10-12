package live

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sse"
)

func counterView(key string, renders *atomic.Int64) View {
	return text(key, []string{"counter"}, func(ctx context.Context, tx *sql.Tx) (string, error) {
		if renders != nil {
			renders.Add(1)
		}
		n, err := count(ctx, tx)
		return strconv.Itoa(n), err
	})
}

// A burst of commits after a quiet spell renders at once, then once more
// for whatever came during that tick: never once per commit. The hub sees
// a commit only through Changed, so here a commit is an increment.
func TestBurstOfCommitsRendersAtMostTwice(t *testing.T) {
	h := start(t, Options{Tick: 50 * time.Millisecond})
	var state, renders atomic.Int64
	v := text("n", []string{"counter"}, func(context.Context, *sql.Tx) (string, error) {
		renders.Add(1)
		return strconv.Itoa(int(state.Load())), nil
	})
	c := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the first frame", func() bool { return c.has(`<p id="n">0</p>`) })
	time.Sleep(100 * time.Millisecond)
	renders.Store(0)

	begin := time.Now()
	for range 100 {
		state.Add(1)
		h.Changed([]string{"counter"})
	}
	if took := time.Since(begin); took > 10*time.Millisecond {
		t.Fatalf("100 commits took %v", took)
	}
	waitFor(t, "the last commit", func() bool { return c.has(`<p id="n">100</p>`) })
	time.Sleep(120 * time.Millisecond)
	if n := renders.Load(); n > 2 {
		t.Fatalf("%d renders for 100 commits", n)
	}
}

// Every stream on a view gets the same frame from one render.
func TestManyStreamsShareOneRender(t *testing.T) {
	write, read := testDB(t, 4)
	h := start(t, Options{Read: read, Workers: 2})
	var renders atomic.Int64
	v := counterView("n", &renders)
	cs := make([]*client, 50)
	for i := range cs {
		cs[i] = serve(h, Spec{Views: []View{v}}, "")
	}
	for _, c := range cs {
		waitFor(t, "the first frame", func() bool { return c.has(`<p id="n">0</p>`) })
	}
	increment(t, write)
	h.Changed([]string{"counter"})
	for _, c := range cs {
		waitFor(t, "the second frame", func() bool { return c.has(`<p id="n">1</p>`) })
	}
	s := h.Stats()
	if renders.Load() != 2 || s.Renders != 2 || s.Ticks != 2 || s.FramesSent != 100 {
		t.Fatalf("%d renders, stats %+v", renders.Load(), s)
	}
	for _, c := range cs {
		if p := c.patches(); len(p) != 2 {
			t.Fatalf("a stream got %q", p)
		}
	}
}

// A tick's views are shared out among the workers, and each worker
// renders all of its views in one read transaction, which ends with the
// tick: one snapshot per worker, even while commits land.
func TestOneReadTransactionPerWorkerPerTick(t *testing.T) {
	write, read := testDB(t, 8)
	const workers, views = 4, 40
	h := start(t, Options{Read: read, Workers: workers})
	var mu sync.Mutex
	seen := map[*sql.Tx][]int{} // what each transaction read
	vs := make([]View, views)
	for i := range vs {
		vs[i] = text(fmt.Sprint("v", i), []string{"counter"}, func(ctx context.Context, tx *sql.Tx) (string, error) {
			n, err := count(ctx, tx)
			time.Sleep(time.Millisecond)
			mu.Lock()
			seen[tx] = append(seen[tx], n)
			mu.Unlock()
			return strconv.Itoa(n), err
		})
	}
	serve(h, Spec{Views: vs}, "")
	waitFor(t, "the first tick", func() bool { s := h.Stats(); return s.Renders == views && s.Ticks > 0 })
	if n := h.Stats().Ticks; n != 1 {
		t.Fatalf("the views took %d ticks", n)
	}
	mu.Lock()
	clear(seen)
	mu.Unlock()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				write.Exec(`update counter set n = n + 1`)
			}
		}
	})
	h.Changed([]string{"counter"})
	waitFor(t, "the second tick", func() bool { s := h.Stats(); return s.Renders == 2*views && s.Ticks > 1 })
	close(stop)
	wg.Wait()

	if n := h.Stats().Ticks; n != 2 {
		t.Fatalf("%d ticks", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || len(seen) > workers {
		t.Fatalf("%d read transactions for %d workers", len(seen), workers)
	}
	for tx, ns := range seen {
		for _, n := range ns {
			if n != ns[0] {
				t.Fatalf("one transaction read %v", ns)
			}
		}
		if err := tx.QueryRow(`select 1`).Scan(new(int)); !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("a read transaction outlived its tick: %v", err)
		}
	}
}

// A client that stops reading skips frames and loses its stream at the
// write deadline. Meanwhile the others get every frame: the stalled
// stream holds neither a lock nor a compression slot.
func TestStalledClientHoldsUpNoOne(t *testing.T) {
	write, read := testDB(t, 4)
	h := start(t, Options{Read: read, Workers: 2, Tick: 5 * time.Millisecond,
		SSE: sse.Options{Compress: make(chan struct{}, 1), WriteTimeout: time.Second}})
	v := counterView("n", nil)
	slow := serve(h, Spec{Views: []View{v}}, "zstd")
	fast := serve(h, Spec{Views: []View{v}}, "zstd")
	got := func(c *client, html string) func() bool {
		return func() bool { return strings.Contains(string(unzstd(t, c.bytes())), html) }
	}
	waitFor(t, "the first frames", func() bool { return got(slow, `<p id="n">0</p>`)() && got(fast, `<p id="n">0</p>`)() })

	slow.stall()
	for i := 1; i <= 20; i++ {
		increment(t, write)
		h.Changed([]string{"counter"})
		waitFor(t, "the healthy stream", got(fast, fmt.Sprintf(`<p id="n">%d</p>`, i)))
	}
	if h.Stats().FramesDropped == 0 {
		t.Fatal("the stalled stream dropped no frames")
	}
	select {
	case err := <-slow.done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("the stalled stream ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled stream outlived its write deadline")
	}
	increment(t, write)
	h.Changed([]string{"counter"})
	waitFor(t, "the healthy stream", got(fast, `<p id="n">21</p>`))
}

// leavingClient goes away during a write once leaving is set: like
// net/http, it ends the request's context before the write fails.
type leavingClient struct {
	*client
	leaving atomic.Bool
}

func (c *leavingClient) Write(p []byte) (int, error) {
	if c.leaving.Load() {
		c.cancel()
		return 0, errors.New("write: broken pipe")
	}
	return c.client.Write(p)
}

// A client that leaves during a write is a client that leaves: Serve
// returns nil, not the write's error.
func TestClientLeavingMidWrite(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	var n atomic.Int64
	v := text("n", []string{"n"}, func(context.Context, *sql.Tx) (string, error) {
		return strconv.Itoa(int(n.Load())), nil
	})
	var lc *leavingClient
	c := serveThrough(h, Spec{Views: []View{v}}, "", func(c *client) http.ResponseWriter {
		lc = &leavingClient{client: c}
		return lc
	})
	waitFor(t, "the first frame", func() bool { return c.has(`<p id="n">0</p>`) })
	lc.leaving.Store(true)
	n.Store(1)
	h.Changed([]string{"n"})
	if err := c.returned(t); err != nil {
		t.Fatalf("Serve returned %v", err)
	}
}

func TestMaxStreams(t *testing.T) {
	h := start(t, Options{MaxStreams: 2})
	v := text("a", nil, func(context.Context, *sql.Tx) (string, error) { return "a", nil })
	a := serve(h, Spec{Views: []View{v}}, "")
	b := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "two streams", func() bool { return a.has(`<p id="a">a</p>`) && b.has(`<p id="a">a</p>`) })
	c := serve(h, Spec{Views: []View{v}}, "")
	select {
	case err := <-c.done:
		if !errors.Is(err, ErrFull) || c.status() != http.StatusServiceUnavailable {
			t.Fatalf("a third stream: %v, status %d", err, c.status())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a third stream was served")
	}
	a.leave(t)
	d := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "a stream in the freed place", func() bool { return d.has(`<p id="a">a</p>`) })
}

func TestWatchAndUnwatch(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	var n atomic.Int64
	panel := text("panel", []string{"panel"}, func(context.Context, *sql.Tx) (string, error) {
		return strconv.Itoa(int(n.Load())), nil
	})
	page := text("page", nil, func(context.Context, *sql.Tx) (string, error) { return "page", nil })
	c := serve(h, Spec{ID: "tab1", Views: []View{page}}, "")
	waitFor(t, "the page", func() bool { return c.has(`<p id="page">page</p>`) })

	if h.Watch("tab2", panel) {
		t.Fatal("watched for a stream that is not open")
	}
	if !h.Watch("tab1", panel) {
		t.Fatal("could not watch")
	}
	waitFor(t, "the panel", func() bool { return c.has(`<p id="panel">0</p>`) })
	n.Store(1)
	h.Changed([]string{"panel"})
	waitFor(t, "the panel again", func() bool { return c.has(`<p id="panel">1</p>`) })

	if !h.Unwatch("tab1", "panel") || h.Unwatch("tab1", "panel") {
		t.Fatal("Unwatch")
	}
	if h.Count("panel") != 0 || h.Stats().Views != 1 {
		t.Fatalf("the panel is still there: %+v", h.Stats())
	}
	n.Store(2)
	h.Changed([]string{"panel"})
	time.Sleep(50 * time.Millisecond)
	if c.has(`<p id="panel">2</p>`) {
		t.Fatal("got the panel after Unwatch")
	}

	// Watching it again starts from its current state.
	h.Watch("tab1", panel)
	waitFor(t, "the panel after watching again", func() bool { return c.has(`<p id="panel">2</p>`) })
}

// When the hub stops, every stream ends at once, even one stuck in a
// write to a client that stopped reading.
func TestShutdownWaitsForNoStream(t *testing.T) {
	_, read := testDB(t, 4)
	h := New(Options{Read: read, Workers: 2, Log: slog.New(slog.DiscardHandler), SSE: sse.Options{WriteTimeout: time.Minute}})
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- h.Run(ctx) }()
	var n atomic.Int64
	v := text("n", []string{"n"}, func(context.Context, *sql.Tx) (string, error) {
		return strconv.Itoa(int(n.Load())), nil
	})
	cs := make([]*client, 10)
	for i := range cs {
		cs[i] = serve(h, Spec{Views: []View{v}}, "")
		waitFor(t, "the first frame", func() bool { return cs[i].has(`<p id="n">0</p>`) })
	}
	stuck := cs[0]
	stuck.stall()
	n.Store(1)
	h.Changed([]string{"n"})
	<-stuck.blocked

	begin := time.Now()
	cancel()
	select {
	case err := <-ran:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	for _, c := range cs {
		select {
		case err := <-c.done:
			if err != nil {
				t.Fatalf("Serve returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a stream outlived the hub")
		}
	}
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("shutdown took %v", d)
	}
	late := serve(h, Spec{Views: []View{v}}, "")
	if err := <-late.done; !errors.Is(err, ErrStopped) || late.status() != http.StatusServiceUnavailable {
		t.Fatalf("a stream after the hub stopped: %v, status %d", err, late.status())
	}
}

// stoppingClient stops the hub while the stream flushes its headers, and
// the flush fails as the stream ends with it.
type stoppingClient struct {
	*client
	h *Hub
}

func (c *stoppingClient) FlushError() error {
	c.h.stop()
	return context.Canceled
}

// A stream that opens while the hub stops gets a plain 503, without the
// Content-Encoding the stream had set.
func TestStopWhileOpening(t *testing.T) {
	h := New(Options{Log: slog.New(slog.DiscardHandler)})
	v := text("v", nil, func(context.Context, *sql.Tx) (string, error) { return "v", nil })
	c := serveThrough(h, Spec{Views: []View{v}}, "zstd", func(c *client) http.ResponseWriter {
		return &stoppingClient{client: c, h: h}
	})
	if err := c.returned(t); !errors.Is(err, ErrStopped) || c.status() != http.StatusServiceUnavailable {
		t.Fatalf("Serve returned %v, status %d", err, c.status())
	}
	if e := c.header.Get("Content-Encoding"); e != "" {
		t.Fatalf("the 503 says Content-Encoding %q", e)
	}
}

// A commit after a quiet spell renders at once; the next tick waits for
// the tick interval.
func TestLeadingEdgeThenTick(t *testing.T) {
	h := start(t, Options{Tick: 200 * time.Millisecond})
	var mu sync.Mutex
	var at []time.Time
	v := View{Key: "v", Topics: []string{"t"}, Render: func(context.Context, *sql.Tx) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		at = append(at, time.Now())
		return sse.PatchElements(fmt.Appendf(nil, `<p id="v">%d</p>`, len(at))), nil
	}}
	renders := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(at)
	}
	serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the first render", func() bool { return renders() == 1 })
	time.Sleep(250 * time.Millisecond)

	changed := time.Now()
	h.Changed([]string{"t"})
	waitFor(t, "the second render", func() bool { return renders() == 2 })
	h.Changed([]string{"t"})
	waitFor(t, "the third render", func() bool { return renders() == 3 })
	mu.Lock()
	defer mu.Unlock()
	if d := at[1].Sub(changed); d > 100*time.Millisecond {
		t.Fatalf("rendered %v after a commit that followed a quiet spell", d)
	}
	if d := at[2].Sub(at[1]); d < 180*time.Millisecond {
		t.Fatalf("two ticks %v apart", d)
	}
}

func TestEvery(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	var n atomic.Int64
	v := View{Key: "clock", Every: 30 * time.Millisecond, Render: func(context.Context, *sql.Tx) (any, error) {
		return sse.PatchElements(fmt.Appendf(nil, `<p id="clock">%d</p>`, n.Add(1))), nil
	}}
	c := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the first frame", func() bool { return c.has(`<p id="clock">1</p>`) })
	time.Sleep(300 * time.Millisecond)
	if got := n.Load(); got < 6 || got > 13 {
		t.Fatalf("%d renders in 300 ms, every 30 ms", got)
	}
	waitFor(t, "a later frame", func() bool { return c.has(`<p id="clock">6</p>`) })
}

func TestPresence(t *testing.T) {
	var h *Hub
	h = start(t, Options{Tick: 5 * time.Millisecond})
	v := text("here", nil, func(context.Context, *sql.Tx) (string, error) { return strconv.Itoa(h.Count("here")), nil })
	v.Presence = true
	last := func(c *client) string {
		p := c.patches()
		if len(p) == 0 {
			return ""
		}
		return p[len(p)-1]
	}
	a := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "one present", func() bool { return last(a) == `<p id="here">1</p>` })
	b := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "two present", func() bool { return last(a) == `<p id="here">2</p>` && last(b) == `<p id="here">2</p>` })
	b.leave(t)
	waitFor(t, "one left", func() bool { return last(a) == `<p id="here">1</p>` })
	a.leave(t)
	if s := h.Stats(); s.Views != 0 || s.Streams != 0 {
		t.Fatalf("left behind: %+v", s)
	}
}

// A failed render, or one that panics, is logged and keeps the frame the
// page has.
func TestRenderFailureKeepsFrame(t *testing.T) {
	l := &logs{}
	h := start(t, Options{Tick: 5 * time.Millisecond, Log: l.logger()})
	var calls atomic.Int64
	v := View{Key: "v", Topics: []string{"t"}, Render: func(context.Context, *sql.Tx) (any, error) {
		switch n := calls.Add(1); n {
		case 2:
			return nil, errors.New("boom")
		case 3:
			panic("bang")
		default:
			return sse.PatchElements(fmt.Appendf(nil, `<p id="v">%d</p>`, n)), nil
		}
	}}
	c := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the first frame", func() bool { return c.has(`<p id="v">1</p>`) })
	h.Changed([]string{"t"})
	waitFor(t, "the error", func() bool { return strings.Contains(l.String(), "boom") })
	h.Changed([]string{"t"})
	waitFor(t, "the panic", func() bool { return strings.Contains(l.String(), "bang") })
	h.Changed([]string{"t"})
	waitFor(t, "the next frame", func() bool { return c.has(`<p id="v">4</p>`) })
	if p := c.patches(); len(p) != 2 {
		t.Fatalf("got %q", p)
	}
}

// A view whose first render failed renders again for the next stream
// that watches it.
func TestJoinRetriesViewWithoutFrame(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	var calls atomic.Int64
	v := View{Key: "v", Render: func(context.Context, *sql.Tx) (any, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("not yet")
		}
		return sse.PatchElements([]byte(`<p id="v">ok</p>`)), nil
	}}
	serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the failed render", func() bool { return calls.Load() == 1 })
	c := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the frame", func() bool { return c.has(`<p id="v">ok</p>`) })
}

func TestUnchangedFrameIsNotSent(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	v := text("v", []string{"t"}, func(context.Context, *sql.Tx) (string, error) { return "same", nil })
	c := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the first frame", func() bool { return c.has(`<p id="v">same</p>`) })
	for range 3 {
		h.Changed([]string{"t"})
		time.Sleep(20 * time.Millisecond)
	}
	if s := h.Stats(); s.Renders != 4 || s.FramesSent != 1 || len(c.patches()) != 1 {
		t.Fatalf("stats %+v, patches %q", s, c.patches())
	}
}

// A frame that is not []byte, on a stream without Write, is logged and
// not counted as sent.
func TestFramesSentCountsWrittenFrames(t *testing.T) {
	l := &logs{}
	h := start(t, Options{Tick: 5 * time.Millisecond, Log: l.logger()})
	odd := View{Key: "odd", Render: func(context.Context, *sql.Tx) (any, error) { return "not bytes", nil }}
	v := text("v", nil, func(context.Context, *sql.Tx) (string, error) { return "v", nil })
	c := serve(h, Spec{Views: []View{odd, v}}, "")
	waitFor(t, "a frame sent", func() bool { return h.Stats().FramesSent > 0 })
	if !c.has(`<p id="v">v</p>`) || !strings.Contains(l.String(), "not []byte") {
		t.Fatalf("got %q, logged %s", c.patches(), l.String())
	}
	if n := h.Stats().FramesSent; n != 1 {
		t.Fatalf("%d frames sent", n)
	}
}

func TestHeartbeat(t *testing.T) {
	h := start(t, Options{Heartbeat: 20 * time.Millisecond})
	v := text("v", nil, func(context.Context, *sql.Tx) (string, error) { return "v", nil })
	c := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "heartbeats", func() bool {
		n := 0
		for _, e := range c.events() {
			if e.name == "comment" {
				n++
			}
		}
		return n >= 3
	})
}

// A stream whose frames all tailor to nothing still sends heartbeats.
func TestHeartbeatWhenWriteSendsNothing(t *testing.T) {
	h := start(t, Options{Tick: 2 * time.Millisecond, Heartbeat: 30 * time.Millisecond})
	var n atomic.Int64
	v := View{Key: "v", Every: 5 * time.Millisecond, Render: func(context.Context, *sql.Tx) (any, error) {
		return n.Add(1), nil
	}}
	c := serve(h, Spec{Views: []View{v}, Write: func(*sse.Stream, string, any) error { return nil }}, "")
	waitFor(t, "heartbeats", func() bool { return len(c.events()) >= 3 })
	if n.Load() < 6 {
		t.Fatalf("only %d frames", n.Load())
	}
}

// Views that change in the same tick reach a stream together, in the
// order it watches them.
func TestViewsOfOneTickGoInOrder(t *testing.T) {
	h := start(t, Options{Tick: 2 * time.Millisecond, Workers: 4})
	var n atomic.Int64
	views := make([]View, 5)
	for i := range views {
		views[i] = text(fmt.Sprint("v", 4-i), []string{"t"}, func(context.Context, *sql.Tx) (string, error) {
			return strconv.Itoa(int(n.Load())), nil
		})
	}
	c := serve(h, Spec{Views: views}, "")
	for i := 1; i <= 20; i++ {
		n.Store(int64(i))
		h.Changed([]string{"t"})
		waitFor(t, "the tick's frames", func() bool { return c.has(fmt.Sprintf(`<p id="v0">%d</p>`, i)) })
	}
	p := c.patches()
	for i := 0; i+5 <= len(p); i += 5 {
		for j := range 5 {
			if !strings.HasPrefix(p[i+j], fmt.Sprintf(`<p id="v%d">`, 4-j)) {
				t.Fatalf("out of order: %q", p[i:i+5])
			}
		}
	}
}

// blockingWrite records the frames a stream writes as key and value, and
// blocks on the frame first until release is closed.
type blockingWrite struct {
	first            string
	entered, release chan struct{}

	mu  sync.Mutex
	got []string
}

func newBlockingWrite(first string) *blockingWrite {
	return &blockingWrite{first: first, entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingWrite) write(_ *sse.Stream, key string, frame any) error {
	f := fmt.Sprint(key, frame)
	b.mu.Lock()
	b.got = append(b.got, f)
	b.mu.Unlock()
	if f == b.first {
		close(b.entered)
		<-b.release
	}
	return nil
}

func (b *blockingWrite) sent() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.got)
}

// numbers returns views that render what n holds, on topic "t".
func numbers(n *atomic.Int64, keys ...string) []View {
	vs := make([]View, len(keys))
	for i, k := range keys {
		vs[i] = View{Key: k, Topics: []string{"t"}, Render: func(context.Context, *sql.Tx) (any, error) { return n.Load(), nil }}
	}
	return vs
}

// While a stream's Write blocks, the frames of later ticks wait: each
// tick's frames go out together and in the stream's order, and a frame
// that a newer one replaced before it went out counts as dropped.
func TestBlockedWriteKeepsTicksTogether(t *testing.T) {
	h := start(t, Options{Tick: 2 * time.Millisecond})
	var n atomic.Int64
	n.Store(1)
	b := newBlockingWrite("a1")
	serve(h, Spec{Views: numbers(&n, "a", "b"), Write: b.write}, "")
	<-b.entered
	for i := int64(2); i <= 3; i++ {
		n.Store(i)
		h.Changed([]string{"t"})
		waitFor(t, "the tick", func() bool { return h.Stats().Ticks == i })
	}
	waitFor(t, "the third tick's frames", func() bool { return h.Stats().FramesDropped == 2 })
	close(b.release)
	waitFor(t, "the last frames", func() bool {
		got := b.sent()
		return slices.Contains(got, "a3") && slices.Contains(got, "b3")
	})
	if got, want := b.sent(), []string{"a1", "b1", "a3", "b3"}; !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	waitFor(t, "the count", func() bool { return h.Stats().FramesSent == 4 })
	if s := h.Stats(); s.FramesDropped != 2 {
		t.Fatalf("stats %+v", s)
	}
}

// Once Unwatch returns, the stream sends no frame of the view that it has
// not started to write, not even one it took along with a frame it is
// still writing.
func TestNoFrameAfterUnwatch(t *testing.T) {
	h := start(t, Options{Tick: 2 * time.Millisecond})
	var n atomic.Int64
	n.Store(1)
	b := newBlockingWrite("a1")
	serve(h, Spec{ID: "tab", Views: numbers(&n, "a", "b"), Write: b.write}, "")
	<-b.entered
	if !h.Unwatch("tab", "b") {
		t.Fatal("Unwatch")
	}
	close(b.release)
	n.Store(2)
	h.Changed([]string{"t"})
	waitFor(t, "the next frame", func() bool { return slices.Contains(b.sent(), "a2") })
	if got, want := b.sent(), []string{"a1", "a2"}; !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
}

// With Write, the stream tailors each frame: here it sends only the rows
// of a list that it has not sent yet.
func TestWriteTailorsFrames(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	var mu sync.Mutex
	rows := []string{"a", "b", "c"}
	list := View{Key: "list", Topics: []string{"list"}, Render: func(context.Context, *sql.Tx) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(rows), nil
	}}
	tailored := func() func(*sse.Stream, string, any) error {
		var had []string
		return func(s *sse.Stream, key string, frame any) error {
			if key != "list" {
				return fmt.Errorf("key %q", key)
			}
			now := frame.([]string)
			var events [][]byte
			for i, r := range now {
				if i >= len(had) || had[i] != r {
					events = append(events, sse.PatchElements(fmt.Appendf(nil, `<li id="r%d">%s</li>`, i, r)))
				}
			}
			had = now
			return s.Write(events...)
		}
	}
	a := serve(h, Spec{Views: []View{list}, Write: tailored()}, "")
	waitFor(t, "the list", func() bool { return len(a.patches()) == 3 })
	mu.Lock()
	rows = []string{"a", "B", "c"}
	mu.Unlock()
	h.Changed([]string{"list"})
	waitFor(t, "the changed row", func() bool { return a.has(`<li id="r1">B</li>`) })
	if p := a.patches(); len(p) != 4 {
		t.Fatalf("got %q", p)
	}
	b := serve(h, Spec{Views: []View{list}, Write: tailored()}, "")
	waitFor(t, "the whole list for a new stream", func() bool { return len(b.patches()) == 3 && b.has(`<li id="r1">B</li>`) })
}

// A new stream with the ID of an open one replaces it.
func TestSameIDReplacesStream(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	v := text("v", nil, func(context.Context, *sql.Tx) (string, error) { return "v", nil })
	w := text("w", nil, func(context.Context, *sql.Tx) (string, error) { return "w", nil })
	old := serve(h, Spec{ID: "tab", Views: []View{v}}, "")
	waitFor(t, "the old stream", func() bool { return old.has(`<p id="v">v</p>`) })
	cur := serve(h, Spec{ID: "tab", Views: []View{v}}, "")
	select {
	case err := <-old.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the old stream stayed open")
	}
	waitFor(t, "the new stream", func() bool { return cur.has(`<p id="v">v</p>`) })
	if !h.Watch("tab", w) {
		t.Fatal("Watch")
	}
	waitFor(t, "the watched view", func() bool { return cur.has(`<p id="w">w</p>`) })
	if h.Count("v") != 1 {
		t.Fatalf("%d streams on v", h.Count("v"))
	}
}

// slowOpen holds the stream's first flush, which sends its headers, until
// release is closed.
type slowOpen struct {
	*client
	entered, release chan struct{}
	once             sync.Once
}

func (c *slowOpen) FlushError() error {
	c.once.Do(func() {
		close(c.entered)
		<-c.release
	})
	return nil
}

// Of two streams with the same ID, the later request stays, even when
// the earlier one takes longer to open.
func TestSameIDKeepsLaterStream(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	v := text("v", nil, func(context.Context, *sql.Tx) (string, error) { return "v", nil })
	w := text("w", nil, func(context.Context, *sql.Tx) (string, error) { return "w", nil })
	slow := &slowOpen{entered: make(chan struct{}), release: make(chan struct{})}
	earlier := serveThrough(h, Spec{ID: "tab", Views: []View{v}}, "", func(c *client) http.ResponseWriter {
		slow.client = c
		return slow
	})
	<-slow.entered
	later := serve(h, Spec{ID: "tab", Views: []View{v}}, "")
	waitFor(t, "the later stream", func() bool { return later.has(`<p id="v">v</p>`) })
	close(slow.release)
	select {
	case err := <-earlier.done:
		if err != nil {
			t.Fatal(err)
		}
	case err := <-later.done:
		t.Fatalf("the earlier stream replaced the later one, which returned %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("both streams stayed open")
	}
	if !h.Watch("tab", w) {
		t.Fatal("Watch")
	}
	waitFor(t, "the watched view", func() bool { return later.has(`<p id="w">w</p>`) })
	if n := h.Count("v"); n != 1 {
		t.Fatalf("%d streams on v", n)
	}
}

// After a tick runs over, the hub samples tick durations for a while and
// then logs their distribution.
func TestOverrunReport(t *testing.T) {
	l := &logs{}
	h := New(Options{Tick: 5 * time.Millisecond, Log: l.logger()})
	h.sampleFor = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)
	v := View{Key: "slow", Topics: []string{"t"}, Render: func(context.Context, *sql.Tx) (any, error) {
		time.Sleep(15 * time.Millisecond)
		return nil, nil
	}}
	serve(h, Spec{Views: []View{v}}, "")
	for range 5 {
		h.Changed([]string{"t"})
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(t, "the report", func() bool { return strings.Contains(l.String(), "tick overrun") })
	for _, k := range []string{"n=", "p50=", "p95=", "p99=", "max="} {
		if !strings.Contains(l.String(), k) {
			t.Fatalf("no %s in %s", k, l.String())
		}
	}
	if h.Stats().Overruns == 0 {
		t.Fatal("no overruns counted")
	}
}

func TestRunTwice(t *testing.T) {
	h := start(t, Options{})
	time.Sleep(10 * time.Millisecond)
	if err := h.Run(context.Background()); err == nil {
		t.Fatal("ran twice")
	}
}
