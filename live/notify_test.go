package live

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sse"
)

func notice(s string) []byte {
	return sse.PatchElements([]byte(`<p id="outcome">` + s + `</p>`))
}

// A notice goes out once, after the frames of the stream's views, and
// never again: not with the view's next frame, nor to the stream that
// replaces this one.
func TestNotify(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	var n sync.Mutex
	i := 0
	page := text("page", []string{"page"}, func(context.Context, *sql.Tx) (string, error) {
		n.Lock()
		defer n.Unlock()
		return strconv.Itoa(i), nil
	})
	c := serve(h, Spec{ID: "tab", Views: []View{page}}, "")
	waitFor(t, "the page", func() bool { return c.has(`<p id="page">0</p>`) })

	if !h.Notify("tab", "notice", notice("busy")) {
		t.Fatal("Notify did not find the open stream")
	}
	if h.Notify("", "notice", notice("busy")) || h.Notify("tab", "notice", nil) {
		t.Fatal("Notify reported a stream without an ID, or sent no frame")
	}
	waitFor(t, "the notice", func() bool { return c.has(`<p id="outcome">busy</p>`) })

	n.Lock()
	i = 1
	n.Unlock()
	h.Changed([]string{"page"})
	waitFor(t, "the next frame", func() bool { return c.has(`<p id="page">1</p>`) })
	time.Sleep(20 * time.Millisecond)
	if got := c.patches(); !slices.Equal(got, []string{`<p id="page">0</p>`, `<p id="outcome">busy</p>`, `<p id="page">1</p>`}) {
		t.Fatalf("got %q", got)
	}
	if s := h.Stats(); s.FramesSent != 2 || s.Views != 1 {
		t.Fatalf("the notice counted as a frame or a view: %+v", s)
	}

	c.leave(t)
	again := serve(h, Spec{ID: "tab", Views: []View{page}}, "")
	waitFor(t, "the page", func() bool { return again.has(`<p id="page">1</p>`) })
	time.Sleep(20 * time.Millisecond)
	if again.has(`<p id="outcome">busy</p>`) {
		t.Fatal("the next stream with the ID got the notice")
	}
}

// A notice that has not gone out yet gives way to a later one with its
// key; notices with other keys go out after it, in the order of Notify,
// through WriteTick after the frames of the views.
func TestNotifyReplacesByKey(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	gate := make(chan struct{})
	var mu sync.Mutex
	var calls [][]string
	first := true
	tick := func(s *sse.Stream, frames []Frame) error {
		mu.Lock()
		wait := first
		first = false
		mu.Unlock()
		if wait {
			<-gate
		}
		var got []string
		for _, f := range frames {
			got = append(got, fmt.Sprintf("%s=%s", f.Key, f.Value))
		}
		mu.Lock()
		calls = append(calls, got)
		mu.Unlock()
		return nil
	}
	page := View{Key: "page", Render: func(context.Context, *sql.Tx) (any, error) { return "page", nil }}
	serve(h, Spec{ID: "tab", Views: []View{page}, WriteTick: tick}, "")
	waitFor(t, "the first write", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return !first
	})
	for _, n := range [][2]string{{"a", "1"}, {"b", "2"}, {"a", "3"}} {
		if !h.Notify("tab", n[0], n[1]) {
			t.Fatal("Notify did not find the stream")
		}
	}
	close(gate)
	waitFor(t, "the notices", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(calls) == 2
	})
	mu.Lock()
	defer mu.Unlock()
	if want := [][]string{{"page=page"}, {"a=3", "b=2"}}; !slices.EqualFunc(calls, want, slices.Equal) {
		t.Fatalf("WriteTick got %q, want %q", calls, want)
	}
}

// Spec.Write gets a notice under its key.
func TestNotifyThroughWrite(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	var mu sync.Mutex
	var keys []string
	write := func(s *sse.Stream, key string, frame any) error {
		mu.Lock()
		keys = append(keys, key)
		mu.Unlock()
		return s.Write(frame.([]byte))
	}
	page := text("page", nil, func(context.Context, *sql.Tx) (string, error) { return "page", nil })
	c := serve(h, Spec{ID: "tab", Views: []View{page}, Write: write}, "")
	waitFor(t, "the page", func() bool { return c.has(`<p id="page">page</p>`) })
	h.Notify("tab", "notice", notice("busy"))
	waitFor(t, "the notice", func() bool { return c.has(`<p id="outcome">busy</p>`) })
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(keys, []string{"page", "notice"}) {
		t.Fatalf("Write got the keys %q", keys)
	}
}

// A notice for a stream that is not open does nothing: the stream that
// opens with the ID later never gets it, and it takes no place among the
// views that wait for their stream.
func TestNotifyWithoutAStream(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond, MaxStreams: 2})
	page := text("page", nil, func(context.Context, *sql.Tx) (string, error) { return "page", nil })
	for _, id := range []string{"x", "y", "tab"} {
		if h.Notify(id, "notice", notice("early")) {
			t.Fatalf("Notify reported a stream %s that is not open", id)
		}
	}
	h.Watch("tab", text("panel", nil, func(context.Context, *sql.Tx) (string, error) { return "panel", nil }))
	c := serve(h, Spec{ID: "tab", Views: []View{page}}, "")
	waitFor(t, "the panel that waited", func() bool { return c.has(`<p id="panel">panel</p>`) })
	time.Sleep(20 * time.Millisecond)
	if c.has(`<p id="outcome">early</p>`) {
		t.Fatal("a notice for a stream that was not open went out")
	}
}

// Notify races streams that open, replace one another and leave, and
// views that change.
func TestNotifyRaces(t *testing.T) {
	h := start(t, Options{Tick: time.Millisecond, PendingWatch: 50 * time.Millisecond})
	page := text("page", []string{"page"}, func(context.Context, *sql.Tx) (string, error) { return "page", nil })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			for ctx.Err() == nil {
				c := serve(h, Spec{ID: "tab" + strconv.Itoa(i%2), Views: []View{page}}, "")
				time.Sleep(time.Millisecond)
				c.leave(t)
			}
		})
		wg.Go(func() {
			for j := 0; ctx.Err() == nil; j++ {
				h.Notify("tab"+strconv.Itoa(j%3), "k"+strconv.Itoa(j%2), notice(strconv.Itoa(j)))
				h.Changed([]string{"page"})
			}
		})
	}
	wg.Wait()
}

// When the hub stops, a stream sends the events of Options.Stopping for
// its spec, before sse.Reconnect when it has Spec.Reconnect; nil sends
// none.
func TestStopping(t *testing.T) {
	stopping := func(spec Spec) [][]byte {
		switch spec.ID {
		case "":
			return nil
		case "panic":
			panic("no notice")
		}
		return [][]byte{notice("Updating, back in a moment.")}
	}
	note, again := string(notice("Updating, back in a moment.")), string(sse.Reconnect("stream"))
	for _, p := range protocols {
		for _, enc := range []string{"identity", sse.Zstd} {
			t.Run(p.name+" "+enc, func(t *testing.T) {
				if rest := shutdownTailStopping(t, p.h2, enc, Spec{ID: "tab", Reconnect: "stream"}, stopping); !strings.HasSuffix(rest, note+again) {
					t.Errorf("the stream ended with %q, want the notice, then Reconnect", rest)
				}
				if rest := shutdownTailStopping(t, p.h2, enc, Spec{ID: "tab"}, stopping); !strings.HasSuffix(rest, note) {
					t.Errorf("a stream without Reconnect ended with %q, want the notice", rest)
				}
				if rest := shutdownTailStopping(t, p.h2, enc, Spec{Reconnect: "stream"}, stopping); strings.Contains(rest, "outcome") || !strings.HasSuffix(rest, again) {
					t.Errorf("a stream for which Stopping has nothing ended with %q", rest)
				}
				if rest := shutdownTailStopping(t, p.h2, enc, Spec{ID: "panic", Reconnect: "stream"}, stopping); strings.Contains(rest, "outcome") || !strings.HasSuffix(rest, again) {
					t.Errorf("a stream whose Stopping panicked ended with %q", rest)
				}
			})
		}
	}
}

// A notice sent right after a commit goes out after the frame that commit
// renders, in the same write, so the frame never overwrites it.
func TestNotifyAfterTheCommitsFrame(t *testing.T) {
	h := start(t, Options{Tick: 20 * time.Millisecond})
	var mu sync.Mutex
	i := 0
	page := text("outcome", []string{"tab"}, func(context.Context, *sql.Tx) (string, error) {
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		return "row " + strconv.Itoa(i), nil
	})
	c := serve(h, Spec{ID: "tab", Views: []View{page}}, "")
	waitFor(t, "the first frame", func() bool { return c.has(`<p id="outcome">row 0</p>`) })
	for n := 1; n <= 5; n++ {
		mu.Lock()
		i = n
		mu.Unlock()
		h.Changed([]string{"tab"}) // a commit, as OnCommit reports it before Do returns
		h.Notify("tab", "notice", notice("notice "+strconv.Itoa(n)))
		frame, want := `<p id="outcome">row `+strconv.Itoa(n)+`</p>`, `<p id="outcome">notice `+strconv.Itoa(n)+`</p>`
		waitFor(t, "the notice", func() bool { return c.has(want) })
		got := c.patches()
		if f, w := slices.Index(got, frame), slices.Index(got, want); f < 0 || f > w {
			t.Fatalf("round %d: the notice went out before the commit's frame: %q", n, got)
		}
	}
}

// The notice goes out in the same write as the frame of the commit it
// waited for.
func TestNotifyInTheFramesWrite(t *testing.T) {
	h := start(t, Options{Tick: 20 * time.Millisecond})
	var mu sync.Mutex
	i := 0
	var writes [][]string
	tick := func(s *sse.Stream, frames []Frame) error {
		var keys []string
		for _, f := range frames {
			keys = append(keys, f.Key)
		}
		mu.Lock()
		writes = append(writes, keys)
		mu.Unlock()
		return nil
	}
	page := View{Key: "outcome", Topics: []string{"tab"}, Render: func(context.Context, *sql.Tx) (any, error) {
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		return i, nil
	}}
	serve(h, Spec{ID: "tab", Views: []View{page}, WriteTick: tick}, "")
	waitFor(t, "the first frame", func() bool { mu.Lock(); defer mu.Unlock(); return len(writes) == 1 })
	for n := 1; n <= 5; n++ {
		mu.Lock()
		i = n
		mu.Unlock()
		h.Changed([]string{"tab"})
		h.Notify("tab", "notice", "notice")
		waitFor(t, "the write", func() bool { mu.Lock(); defer mu.Unlock(); return len(writes) == n+1 })
	}
	mu.Lock()
	defer mu.Unlock()
	for _, w := range writes[1:] {
		if !slices.Equal(w, []string{"outcome", "notice"}) {
			t.Fatalf("writes %q, want the frame and the notice in each", writes)
		}
	}
}

// A render that fails holds a notice no longer than its tick, on a hub
// that gets no further commit (a database that fails): the notice that
// waited for the render goes out once its tick has published, and one sent
// after that tick goes out at once.
func TestNotifyAfterAFailedRender(t *testing.T) {
	h := start(t, Options{Tick: 10 * time.Millisecond})
	var mu sync.Mutex
	row, fail := "row 0", false
	page := text("outcome", []string{"tab"}, func(context.Context, *sql.Tx) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return "", errors.New("the read failed")
		}
		return row, nil
	})
	c := serve(h, Spec{ID: "tab", Views: []View{page}}, "")
	waitFor(t, "the first frame", func() bool { return c.has(`<p id="outcome">row 0</p>`) })
	mu.Lock()
	row, fail = "row 1", true
	mu.Unlock()
	h.Changed([]string{"tab"})
	h.Notify("tab", "held", notice("held"))
	waitFor(t, "the held notice", func() bool { return c.has(`<p id="outcome">held</p>`) })
	time.Sleep(30 * time.Millisecond) // the failed tick has published
	h.Notify("tab", "later", notice("later"))
	deadline := time.Now().Add(200 * time.Millisecond)
	for !c.has(`<p id="outcome">later</p>`) {
		if time.Now().After(deadline) {
			t.Fatal("a notice after the failed render waited for a tick that never comes")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if c.has(`<p id="outcome">row 1</p>`) {
		t.Fatal("the failed render sent a frame")
	}
}

// Notices that wait when the hub stops, held for a render or queued, go
// out before the events of Options.Stopping.
func TestNotifyWhenTheHubStops(t *testing.T) {
	h := New(Options{Tick: 5 * time.Millisecond, Log: slog.New(slog.DiscardHandler), Stopping: func(Spec) [][]byte {
		return [][]byte{notice("stopping")}
	}})
	ctx, stop := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- h.Run(ctx) }()
	var renders sync.Map
	page := text("page", []string{"page"}, func(ctx context.Context, _ *sql.Tx) (string, error) {
		if _, again := renders.LoadOrStore("first", true); !again {
			return "page", nil
		}
		<-ctx.Done() // a render the stop interrupts
		return "", ctx.Err()
	})
	c := serve(h, Spec{ID: "tab", Views: []View{page}, Reconnect: "stream"}, "")
	waitFor(t, "the page", func() bool { return c.has(`<p id="page">page</p>`) })
	h.Changed([]string{"page"})
	time.Sleep(20 * time.Millisecond) // the render has started
	if !h.Notify("tab", "notice", notice("held")) {
		t.Fatal("Notify did not find the stream")
	}
	stop()
	if err := c.returned(t); err != nil {
		t.Fatal(err)
	}
	<-ran
	got := c.patches()
	if len(got) < 3 || got[len(got)-3] != `<p id="outcome">held</p>` || got[len(got)-2] != `<p id="outcome">stopping</p>` {
		t.Fatalf("the stream ended with %q, want the notice, then Stopping's, then Reconnect", got)
	}
}

// A stream keeps MaxNotices notices waiting: one with a new key beyond
// them drops the oldest, queued before held; a known key replaces its own.
func TestNotifyCap(t *testing.T) {
	s := &stream{}
	s.holdNote(note{key: "held", value: "h"}, 1)
	for i := range MaxNotices {
		s.note(note{key: strconv.Itoa(i), value: i})
	}
	if len(s.notes)+len(s.held) != MaxNotices || s.notes[0].key != "1" || len(s.held) != 1 {
		t.Fatalf("%d queued from %q, %d held", len(s.notes), s.notes[0].key, len(s.held))
	}
	s.note(note{key: "5", value: "again"})
	if len(s.notes) != MaxNotices-1 {
		t.Fatalf("a known key took a place: %d queued", len(s.notes))
	}
	s.notes = s.notes[:0]
	for i := range MaxNotices {
		s.holdNote(note{key: "h" + strconv.Itoa(i), value: i}, 1)
	}
	if len(s.held) != MaxNotices || s.held[0].key != "h0" {
		t.Fatalf("%d held from %q", len(s.held), s.held[0].key)
	}
}
