package live

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sse"
)

func mainPatch(s string) [][]byte {
	return [][]byte{sse.PatchElements([]byte(`<main id="main">` + s + `</main>`))}
}

func constant(key, s string) View {
	return text(key, nil, func(context.Context, *sql.Tx) (string, error) { return s, nil })
}

// kinds lists the stream's events as their names and data, without
// comments.
func kinds(c *client) []string {
	var out []string
	for _, e := range c.events() {
		if e.name != "comment" {
			out = append(out, e.name+" "+e.data)
		}
	}
	return out
}

const (
	patchEv   = "datastar-patch-elements "
	signalsEv = "datastar-patch-signals "
)

// A page a former build rendered gets Refresh's events first, then the
// views' first frames, then the build signal set to the hub's build.
func TestRefresh(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond, Build: "new"})
	if h.Build() != "new" {
		t.Fatalf("Build() = %q", h.Build())
	}
	var got RefreshRequest
	spec := Spec{
		ID: "tab", Reconnect: "stream", Views: []View{constant("count", "1")},
		Build: "old", Page: "/boards/7?q=x", Dirty: true,
		Refresh: func(_ context.Context, rq RefreshRequest) ([][]byte, error) {
			got = rq
			return mainPatch("fresh"), nil
		},
	}
	c := serve(h, spec, "")
	waitFor(t, "the build signal", func() bool { return len(kinds(c)) == 3 })
	want := []string{patchEv + `<main id="main">fresh</main>`, patchEv + `<p id="count">1</p>`, signalsEv + `{"_build":"new"}`}
	if k := kinds(c); !slices.Equal(k, want) {
		t.Fatalf("got %q, want %q", k, want)
	}
	if got.Page != "/boards/7?q=x" || got.Build != "old" || !got.Dirty || got.Request == nil {
		t.Fatalf("Refresh got %+v", got)
	}
	if s := h.Stats(); s.Refreshes != 1 || s.FramesSent != 1 {
		t.Fatalf("stats %+v", s)
	}
	c.leave(t)
}

// The first write of a refreshed stream holds Refresh's events alone.
func TestRefreshIsTheFirstWrite(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond, Build: "new"})
	var writes atomic.Int64
	var first atomic.Value
	spec := Spec{
		Views: []View{constant("count", "1")}, Build: "old",
		Refresh: func(context.Context, RefreshRequest) ([][]byte, error) { return mainPatch("fresh"), nil },
	}
	c := serveThrough(h, spec, "", func(c *client) http.ResponseWriter {
		return writeHook{c, func(p []byte) {
			if writes.Add(1) == 1 {
				first.Store(string(p))
			}
		}}
	})
	waitFor(t, "the build signal", func() bool { return len(kinds(c)) == 3 })
	if f, _ := first.Load().(string); f != string(mainPatch("fresh")[0]) {
		t.Fatalf("the first write was %q", f)
	}
	c.leave(t)
}

// writeHook shows each write to f.
type writeHook struct {
	*client
	f func([]byte)
}

func (w writeHook) Write(p []byte) (int, error) {
	w.f(p)
	return w.client.Write(p)
}

// The page's build is the hub's, or the page reports none: Refresh is not
// called and the stream sends its frames alone.
func TestRefreshSameBuild(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond, Build: "new"})
	var calls atomic.Int64
	refresh := func(context.Context, RefreshRequest) ([][]byte, error) {
		calls.Add(1)
		return mainPatch("fresh"), nil
	}
	for _, build := range []string{"new", ""} {
		c := serve(h, Spec{Views: []View{constant("count", "1")}, Build: build, Refresh: refresh}, "")
		waitFor(t, "the frame", func() bool { return c.has(`<p id="count">1</p>`) })
		time.Sleep(20 * time.Millisecond)
		if k := kinds(c); len(k) != 1 {
			t.Errorf("build %q: got %q", build, k)
		}
		c.leave(t)
	}
	if calls.Load() != 0 {
		t.Fatalf("Refresh ran %d times", calls.Load())
	}
	unknown := start(t, Options{Tick: 5 * time.Millisecond})
	if unknown.Build() != "" {
		t.Fatalf("a test binary's build is %q", unknown.Build())
	}
	c := serve(unknown, Spec{Views: []View{constant("count", "1")}, Build: "old", Refresh: refresh}, "")
	waitFor(t, "the frame", func() bool { return c.has(`<p id="count">1</p>`) })
	c.leave(t)
	if calls.Load() != 0 {
		t.Fatal("a hub with no build refreshed")
	}
}

// A Refresh that finds the page current sends nothing but the build
// signal, under its own name.
func TestRefreshUnchanged(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond, Build: "new", BuildSignal: "_v"})
	c := serve(h, Spec{
		Views: []View{constant("count", "1")}, Build: "old",
		Refresh: func(context.Context, RefreshRequest) ([][]byte, error) { return nil, nil },
	}, "")
	waitFor(t, "the build signal", func() bool { return len(kinds(c)) == 2 })
	want := []string{patchEv + `<p id="count">1</p>`, signalsEv + `{"_v":"new"}`}
	if k := kinds(c); !slices.Equal(k, want) {
		t.Fatalf("got %q, want %q", k, want)
	}
	if s := h.Stats(); s.Refreshes != 0 || s.RefreshesUnchanged != 1 {
		t.Fatalf("stats %+v", s)
	}
	c.leave(t)
}

// The build signal waits for the views' first frames, also when they
// render ticks after the stream opened, as a view the hub keeps no frame
// of does.
func TestRefreshBuildAfterSlowFirstFrame(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond, Build: "new", RenderTimeout: time.Minute})
	release := make(chan struct{})
	slow := text("slow", nil, func(ctx context.Context, _ *sql.Tx) (string, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return "late", nil
	})
	c := serve(h, Spec{
		Views: []View{slow}, Build: "old",
		Refresh: func(context.Context, RefreshRequest) ([][]byte, error) { return nil, nil },
	}, "")
	waitFor(t, "the stream", func() bool { return h.Stats().Streams == 1 })
	time.Sleep(100 * time.Millisecond)
	if k := kinds(c); len(k) > 0 {
		t.Fatalf("the stream wrote before the view's first frame: %q", k)
	}
	close(release)
	waitFor(t, "the build signal", func() bool { return len(kinds(c)) == 2 })
	want := []string{patchEv + `<p id="slow">late</p>`, signalsEv + `{"_build":"new"}`}
	if k := kinds(c); !slices.Equal(k, want) {
		t.Fatalf("got %q, want %q", k, want)
	}
	c.leave(t)
}

// nothing is a view whose Render has nothing to send.
func nothing(key string, calls *atomic.Int64) View {
	return View{Key: key, Render: func(context.Context, *sql.Tx) (any, error) {
		calls.Add(1)
		return nil, nil
	}}
}

// A view whose first render brings no frame holds up no build signal,
// which still waits for the frames of the others.
func TestRefreshBuildAfterRenderWithoutFrame(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond, Build: "new", RenderTimeout: time.Minute})
	release := make(chan struct{})
	slow := text("slow", nil, func(ctx context.Context, _ *sql.Tx) (string, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return "late", nil
	})
	var calls atomic.Int64
	refresh := func(context.Context, RefreshRequest) ([][]byte, error) { return nil, nil }
	c := serve(h, Spec{Views: []View{nothing("none", &calls), slow}, Build: "old", Refresh: refresh}, "")
	waitFor(t, "the empty render", func() bool { return calls.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	if k := kinds(c); len(k) > 0 {
		t.Fatalf("the stream wrote before the slow view's first frame: %q", k)
	}
	close(release)
	waitFor(t, "the build signal", func() bool { return len(kinds(c)) == 2 })
	want := []string{patchEv + `<p id="slow">late</p>`, signalsEv + `{"_build":"new"}`}
	if k := kinds(c); !slices.Equal(k, want) {
		t.Fatalf("got %q, want %q", k, want)
	}
	c.leave(t)

	// A stream whose only view has nothing to send gets the build signal.
	c = serve(h, Spec{Views: []View{nothing("none2", &calls)}, Build: "old", Refresh: refresh}, "")
	waitFor(t, "the build signal", func() bool { return len(kinds(c)) == 1 })
	if k := kinds(c); k[0] != signalsEv+`{"_build":"new"}` {
		t.Fatalf("got %q", k)
	}
	c.leave(t)
}

// A first render that fails, or runs past RenderTimeout, brings no frame:
// the build signal follows it and waits for no later render.
func TestRefreshBuildAfterFailedRender(t *testing.T) {
	const timeout = 50 * time.Millisecond
	cases := []struct {
		name   string
		render func(context.Context) error
	}{
		{"error", func(context.Context) error { return errors.New("boom") }},
		{"timeout", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := start(t, Options{Tick: 5 * time.Millisecond, Build: "new", RenderTimeout: timeout})
			var calls atomic.Int64
			failing := View{Key: "failing", Render: func(ctx context.Context, _ *sql.Tx) (any, error) {
				calls.Add(1)
				return nil, tc.render(ctx)
			}}
			began := time.Now()
			c := serve(h, Spec{Views: []View{failing, constant("count", "1")}, Build: "old",
				Refresh: func(context.Context, RefreshRequest) ([][]byte, error) { return nil, nil }}, "")
			waitFor(t, "the build signal", func() bool { return len(kinds(c)) == 2 })
			if d := time.Since(began); d > timeout+time.Second {
				t.Fatalf("the build signal took %v", d)
			}
			want := []string{patchEv + `<p id="count">1</p>`, signalsEv + `{"_build":"new"}`}
			if k := kinds(c); !slices.Equal(k, want) {
				t.Fatalf("got %q, want %q", k, want)
			}
			if n := calls.Load(); n != 1 {
				t.Fatalf("%d renders of the failing view", n)
			}
			c.leave(t)
		})
	}
}

// A refresh that fails, panics, runs past RenderTimeout, or holds the
// stream's element is skipped and counted, and the stream goes on without
// it and without the build signal.
func TestRefreshSkipped(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	cases := []struct {
		name    string
		refresh func(context.Context, RefreshRequest) ([][]byte, error)
		count   func(Stats) int64
		log     string
	}{
		{"error", func(context.Context, RefreshRequest) ([][]byte, error) { return nil, errors.New("boom") },
			func(s Stats) int64 { return s.RefreshErrors }, "boom"},
		{"panic", func(context.Context, RefreshRequest) ([][]byte, error) { panic("bang") },
			func(s Stats) int64 { return s.RefreshErrors }, "bang"},
		{"timeout", func(context.Context, RefreshRequest) ([][]byte, error) { <-release; return mainPatch("late"), nil },
			func(s Stats) int64 { return s.RefreshTimeouts }, "RenderTimeout"},
		{"timeout through ctx", func(ctx context.Context, _ RefreshRequest) ([][]byte, error) { <-ctx.Done(); return nil, ctx.Err() },
			func(s Stats) int64 { return s.RefreshTimeouts }, "RenderTimeout"},
		{"stream element", func(context.Context, RefreshRequest) ([][]byte, error) {
			return [][]byte{sse.PatchElements([]byte(`<body><div id="stream" data-init="@get('/s')"></div></body>`))}, nil
		}, func(s Stats) int64 { return s.RefreshesGuarded }, "second stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var l logs
			// Two slots: a refresh past RenderTimeout holds its slot until it returns.
			h := start(t, Options{Tick: 5 * time.Millisecond, Build: "new", RenderTimeout: 50 * time.Millisecond, RefreshConcurrency: 2, Log: l.logger()})
			c := serve(h, Spec{Reconnect: "stream", Views: []View{constant("count", "1")}, Build: "old", Refresh: tc.refresh}, "")
			waitFor(t, "the frame", func() bool { return c.has(`<p id="count">1</p>`) })
			time.Sleep(20 * time.Millisecond)
			if k := kinds(c); len(k) != 1 {
				t.Fatalf("got %q", k)
			}
			s := h.Stats()
			if tc.count(s) != 1 || s.Refreshes != 0 {
				t.Fatalf("stats %+v", s)
			}
			if !strings.Contains(l.String(), tc.log) {
				t.Fatalf("the log lacks %q: %s", tc.log, l.String())
			}
			c.leave(t)
			// A second one within refreshLogEvery counts, and logs nothing.
			lines := strings.Count(l.String(), "\n")
			c = serve(h, Spec{Reconnect: "stream", Views: []View{constant("count", "1")}, Build: "old", Refresh: tc.refresh}, "")
			waitFor(t, "the second count", func() bool { return tc.count(h.Stats()) == 2 })
			if n := strings.Count(l.String(), "\n"); n != lines {
				t.Fatalf("a second skipped refresh logged again: %s", l.String())
			}
			c.leave(t)
		})
	}
}

// A stream that finds every slot taken for RefreshWait goes on without its
// refresh; once a slot is free, the next one refreshes.
func TestRefreshBusy(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond, Build: "new", RefreshConcurrency: 1, RenderTimeout: 5 * time.Second})
	h.refreshWait = 50 * time.Millisecond
	release := make(chan struct{})
	var started atomic.Bool
	slow := serve(h, Spec{Views: []View{constant("count", "1")}, Build: "old", Refresh: func(context.Context, RefreshRequest) ([][]byte, error) {
		started.Store(true)
		<-release
		return mainPatch("slow"), nil
	}}, "")
	waitFor(t, "the slow refresh", started.Load)
	refresh := func(context.Context, RefreshRequest) ([][]byte, error) { return mainPatch("fresh"), nil }
	c := serve(h, Spec{Views: []View{constant("count", "1")}, Build: "old", Refresh: refresh}, "")
	waitFor(t, "the frame", func() bool { return c.has(`<p id="count">1</p>`) })
	if k := kinds(c); len(k) != 1 {
		t.Fatalf("got %q", k)
	}
	if s := h.Stats(); s.RefreshesBusy != 1 {
		t.Fatalf("stats %+v", s)
	}
	close(release)
	waitFor(t, "the slow stream's refresh", func() bool { return slow.has(`<main id="main">slow</main>`) })
	again := serve(h, Spec{Views: []View{constant("count", "1")}, Build: "old", Refresh: refresh}, "")
	waitFor(t, "the refresh", func() bool { return again.has(`<main id="main">fresh</main>`) })
	c.leave(t)
	slow.leave(t)
	again.leave(t)
}

// A client whose streams all report an old build, as a forged one, holds
// one of the shared slots at most: another client's stream still gets its
// refresh while the first client's refreshes hang. Its next stream waits
// for the client's own slot and is skipped after RefreshWait.
func TestRefreshOneSlotPerClient(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond, Build: "new", RefreshConcurrency: 2, RenderTimeout: 5 * time.Second})
	h.refreshWait = 200 * time.Millisecond
	release := make(chan struct{})
	var running atomic.Int32
	hang := func(context.Context, RefreshRequest) ([][]byte, error) {
		running.Add(1)
		<-release
		return mainPatch("late"), nil
	}
	var forged []*client
	for i := range 3 {
		forged = append(forged, serve(h, Spec{ID: "a" + strconv.Itoa(i), Client: "198.51.100.7", Views: []View{constant("count", "1")}, Build: "forged", Refresh: hang}, ""))
	}
	waitFor(t, "the first refresh", func() bool { return running.Load() == 1 })
	waitFor(t, "the others skipped", func() bool { return h.Stats().RefreshesBusy == 2 })
	if n := running.Load(); n != 1 {
		t.Fatalf("%d refreshes of one client run at once", n)
	}
	other := serve(h, Spec{ID: "b", Client: "203.0.113.9", Views: []View{constant("count", "1")}, Build: "old", Refresh: func(context.Context, RefreshRequest) ([][]byte, error) {
		return mainPatch("fresh"), nil
	}}, "")
	waitFor(t, "the other client's refresh", func() bool { return other.has(`<main id="main">fresh</main>`) })
	close(release)
	waitFor(t, "the client's slot given back", func() bool {
		h.refreshMu.Lock()
		defer h.refreshMu.Unlock()
		return len(h.refreshing) == 0
	})
	other.leave(t)
	for _, c := range forged {
		c.leave(t)
	}
}

// holdsID finds the stream's element however its id attribute is written,
// and nothing that only looks like one.
func TestHoldsID(t *testing.T) {
	for html, want := range map[string]bool{
		`<div id="stream" data-init="@get('/s')"></div>`: true,
		`<div id='stream'></div>`:                        true,
		`<div id=stream></div>`:                          true,
		`<div id=stream data-x></div>`:                   true,
		`<div ID="stream"></div>`:                        true,
		`<div Id = "stream"></div>`:                      true,
		"<div\nid=\"stream\"></div>":                     true,
		`<div class="a"id="stream"></div>`:               true,
		`<div id="streams"></div>`:                       false,
		`<div id="my-stream"></div>`:                     false,
		`<div id=stream2></div>`:                         false,
		`<div data-id="stream"></div>`:                   false,
		`<div aria-labelledby="stream"></div>`:           false,
		`<p>id="stream"</p>`:                             false,
		`<div data-init="x(&#34;stream&#34;)"></div>`:    false,
	} {
		if got := holdsID(sse.PatchElements([]byte(html)), "stream"); got != want {
			t.Errorf("%s: %v, want %v", html, got, want)
		}
	}
}
