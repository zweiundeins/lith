package live

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
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
			h := start(t, Options{Tick: 5 * time.Millisecond, Build: "new", RenderTimeout: 50 * time.Millisecond, Log: l.logger()})
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
