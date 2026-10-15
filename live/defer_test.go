package live

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sse"
)

// While a stream's write blocks, its view renders once more, into the
// stream's slot, and then not at all, however often it changes: each
// change counts as deferred, none as dropped. Once the pump takes the
// waiting frame, the view renders again, at its newest state.
func TestStuckStreamDefersItsView(t *testing.T) {
	h := start(t, Options{Tick: 2 * time.Millisecond})
	var n atomic.Int64
	n.Store(1)
	b := newBlockingWrite("a1")
	serve(h, Spec{Views: byteNumbers(&n, "a"), Write: b.write}, "")
	<-b.entered
	n.Store(2)
	h.Changed([]string{"t"})
	waitFor(t, "the frame in the slot", func() bool { return h.Stats().RendersByKey["a"] == 2 })
	for i := int64(3); i <= 10; i++ {
		n.Store(i)
		h.Changed([]string{"t"})
		waitFor(t, "a deferred render", func() bool { return h.Stats().RendersDeferred == i-2 })
	}
	if s := h.Stats(); s.RendersByKey["a"] != 2 || s.FramesDropped != 0 {
		t.Fatalf("while the write blocked: %+v", s)
	}
	close(b.release)
	waitFor(t, "the newest frame", func() bool { return slices.Contains(b.sent(), "a10") })
	if got, want := b.sent(), []string{"a1", "a2", "a10"}; !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	if s := h.Stats(); s.RendersByKey["a"] != 3 || s.FramesDropped != 0 || s.RendersDeferred != 8 {
		t.Fatalf("after: %+v", s)
	}
}

// A shared view renders at every change while one of its streams keeps
// up: that stream gets every frame, and the stuck one's frames drop.
func TestSharedViewRendersWhileOneKeepsUp(t *testing.T) {
	h := start(t, Options{Tick: 2 * time.Millisecond})
	var n atomic.Int64
	n.Store(1)
	b := newBlockingWrite("v1")
	serve(h, Spec{Views: byteNumbers(&n, "v"), Write: b.write}, "")
	<-b.entered
	fast := newBlockingWrite("")
	serve(h, Spec{Views: byteNumbers(&n, "v"), Write: fast.write}, "")
	waitFor(t, "the second stream's first frame", func() bool { return len(fast.sent()) == 1 })
	for i := int64(2); i <= 10; i++ {
		n.Store(i)
		h.Changed([]string{"t"})
		want := "v" + strconv.FormatInt(i, 10)
		waitFor(t, want, func() bool { return slices.Contains(fast.sent(), want) })
	}
	if s := h.Stats(); s.RendersByKey["v"] != 11 || s.RendersDeferred != 0 || s.FramesDropped != 8 {
		t.Fatalf("%+v", s)
	}
	close(b.release)
	waitFor(t, "the stuck stream's newest frame", func() bool { return slices.Contains(b.sent(), "v10") })
	if got, want := b.sent(), []string{"v1", "v10"}; !slices.Equal(got, want) {
		t.Fatalf("the stuck stream sent %v, want %v", got, want)
	}
}

// A stream that starts to watch a deferred view gets its newest state at
// once. A view of []byte frames that one stream watches keeps no frame,
// so the join renders it; one that two streams watch keeps its last frame,
// which the new stream sends at once, and the render that follows brings
// the newest.
func TestJoinRendersDeferredView(t *testing.T) {
	for _, stuck := range []int{1, 2} {
		h := start(t, Options{Tick: 2 * time.Millisecond})
		var n atomic.Int64
		change := func() {
			n.Add(1)
			h.Changed([]string{"t"})
		}
		renders := func(want int64) func() bool {
			return func() bool { return h.Stats().RendersByKey["v"] == want }
		}
		n.Store(1)
		a := newBlockingWrite("v1")
		serve(h, Spec{Views: byteNumbers(&n, "v"), Write: a.write}, "")
		<-a.entered
		change()
		waitFor(t, "the frame in the first slot", renders(2))
		if stuck == 2 {
			b := newBlockingWrite("v2")
			serve(h, Spec{Views: byteNumbers(&n, "v"), Write: b.write}, "")
			<-b.entered
			change()
			waitFor(t, "the frame in the second slot", renders(4))
		}
		change()
		waitFor(t, "a deferred render", func() bool { return h.Stats().RendersDeferred == 1 })
		late := newBlockingWrite("")
		joined := time.Now()
		serve(h, Spec{Views: byteNumbers(&n, "v"), Write: late.write}, "")
		want := "v" + strconv.FormatInt(n.Load(), 10)
		waitFor(t, "the newest state", func() bool { return slices.Contains(late.sent(), want) })
		if d := time.Since(joined); d > 500*time.Millisecond {
			t.Errorf("%d stuck: the newest state took %v", stuck, d)
		}
	}
}

// A slow stream's own view renders about four times a second, where it
// changes at every tick: its frame waits in the slot for the stream's
// next turn, and the view waits with it.
func TestSlowStreamRendersFourTimesASecond(t *testing.T) {
	h := start(t, Options{Tick: 10 * time.Millisecond})
	c := serve(h, Spec{Views: []View{clock("c")}, Slow: true}, "")
	waitFor(t, "the first frame", func() bool { return len(clockFrames(c, "c")) > 0 })
	r0 := h.Stats().RendersByKey["c"]
	time.Sleep(time.Second)
	s := h.Stats()
	if r := s.RendersByKey["c"] - r0; r < 3 || r > 6 || s.RendersDeferred < 50 {
		t.Fatalf("in a second, %d renders and %d deferred", r, s.RendersDeferred)
	}
	if f := len(clockFrames(c, "c")); f < 4 || f > 7 {
		t.Fatalf("%d frames", f)
	}
}

// After the pump takes a waiting frame, the render it asks for repeats
// that frame when the view did not change, and the stream sends nothing.
func TestDeferredRenderRepeatsNothing(t *testing.T) {
	h := start(t, Options{Tick: 2 * time.Millisecond})
	var n atomic.Int64
	n.Store(1)
	b := newBlockingWrite("a1")
	serve(h, Spec{Views: byteNumbers(&n, "a"), Write: b.write}, "")
	<-b.entered
	n.Store(2)
	h.Changed([]string{"t"})
	waitFor(t, "the frame in the slot", func() bool { return h.Stats().RendersByKey["a"] == 2 })
	h.Changed([]string{"t"})
	waitFor(t, "a deferred render", func() bool { return h.Stats().RendersDeferred == 1 })
	close(b.release)
	waitFor(t, "the render after the slot", func() bool { return h.Stats().RendersByKey["a"] == 3 })
	time.Sleep(20 * time.Millisecond)
	if got, want := b.sent(), []string{"a1", "a2"}; !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
}

// A view with Every, and one with Presence, defer like any other: while
// their only stream is stuck no tick renders, and both render again once
// the stream takes its frames.
func TestEveryAndPresenceDefer(t *testing.T) {
	h := start(t, Options{Tick: 2 * time.Millisecond})
	presence := View{Key: "here", Presence: true}
	presence.Render = func(context.Context, *sql.Tx) (any, error) {
		return []byte(strconv.Itoa(h.Count("here"))), nil
	}
	b := newBlockingWrite("here1")
	serve(h, Spec{ID: "a", Views: []View{presence, clock("c")}, Write: b.write}, "")
	<-b.entered
	waitFor(t, "the clock's frame in the slot", func() bool { return h.Stats().RendersByKey["c"] == 2 })
	other := serve(h, Spec{ID: "b", Views: []View{presence}}, "")
	waitFor(t, "the second stream's frame", func() bool { return string(other.bytes()) == "2" })
	other.leave(t)
	waitFor(t, "the presence view deferred", func() bool { return h.Stats().RendersByKey["here"] == 2 && h.Stats().RendersDeferred > 3 })
	s0 := h.Stats()
	time.Sleep(50 * time.Millisecond)
	if s := h.Stats(); s.Ticks != s0.Ticks || s.RendersByKey["c"] != 2 || s.RendersByKey["here"] != 2 {
		t.Fatalf("while stuck: %+v, before %+v", s, s0)
	}
	close(b.release)
	waitFor(t, "the clock again", func() bool { return h.Stats().RendersByKey["c"] > 4 })
	waitFor(t, "one stream here again", func() bool {
		var here []string
		for _, f := range b.sent() {
			if strings.HasPrefix(f, "here") {
				here = append(here, f)
			}
		}
		return slices.Equal(here, []string{"here1", "here2", "here1"})
	})
}

// A stream whose client never takes another byte costs no render until
// its write deadline ends it.
func TestNeverDrainingStreamCostsNoRenders(t *testing.T) {
	h := start(t, Options{Tick: 2 * time.Millisecond, SSE: sse.Options{WriteTimeout: 300 * time.Millisecond}})
	c := serve(h, Spec{Views: []View{clock("c")}}, "")
	waitFor(t, "the first frames", func() bool { return len(clockFrames(c, "c")) > 0 })
	c.stall()
	<-c.blocked
	waitFor(t, "the frame in the slot", func() bool { return h.Stats().RendersDeferred > 0 })
	r := h.Stats().Renders
	select {
	case err := <-c.done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("the stream ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived its write deadline")
	}
	if s := h.Stats(); s.Views != 0 || s.Renders != r {
		t.Fatalf("%d renders when it stalled, after: %+v", r, s)
	}
}
