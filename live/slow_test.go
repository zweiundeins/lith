package live

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sse"
)

// clock is a view whose frame changes at every tick: <p id=key>n</p>,
// with n counting the renders.
func clock(key string) View {
	var n atomic.Int64
	return View{Key: key, Every: time.Millisecond, Render: func(context.Context, *sql.Tx) (any, error) {
		return sse.PatchElements(fmt.Appendf(nil, `<p id="%s">%d</p>`, key, n.Add(1))), nil
	}}
}

// clockFrames are the numbers of a stream's frames of the clock key, in order.
func clockFrames(c *client, key string) []int {
	var out []int
	for _, p := range c.patches() {
		if v, ok := strings.CutPrefix(p, `<p id="`+key+`">`); ok {
			n, _ := strconv.Atoi(strings.TrimSuffix(v, "</p>"))
			out = append(out, n)
		}
	}
	return out
}

// A slow stream gets a view that changes at every tick four times a second
// at most, the first frame at once and then always the newest; a stream
// beside it gets every tick's.
func TestSlowStreamGetsFewerFrames(t *testing.T) {
	h := start(t, Options{Tick: 10 * time.Millisecond})
	v := clock("c")
	opened := time.Now()
	slow := serve(h, Spec{Views: []View{v}, Slow: true}, "")
	fast := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the slow stream's first frame", func() bool { return len(clockFrames(slow, "c")) > 0 })
	if d := time.Since(opened); d > 200*time.Millisecond {
		t.Errorf("the first frame took %v", d)
	}
	time.Sleep(time.Second)
	s, f := clockFrames(slow, "c"), clockFrames(fast, "c")
	if len(s) < 3 || len(s) > 6 || len(f) < 20 {
		t.Fatalf("in a second, the slow stream got %d frames, the other %d", len(s), len(f))
	}
	for i := 1; i < len(s); i++ {
		if s[i] <= s[i-1]+1 {
			t.Fatalf("the slow stream's frames skip none: %v", s)
		}
	}
	if d := h.Stats().FramesDropped; d < int64(len(f)/2) {
		t.Errorf("%d frames dropped", d)
	}
}

// A view that changes now and then reaches a slow stream at once, even
// right after a frame of a view that changes all the time, and that
// view's newest frame comes with it.
func TestSlowStreamSendsARareViewAtOnce(t *testing.T) {
	h := start(t, Options{Tick: 10 * time.Millisecond})
	var note atomic.Int64
	rare := text("note", []string{"note"}, func(context.Context, *sql.Tx) (string, error) {
		return strconv.FormatInt(note.Load(), 10), nil
	})
	c := serve(h, Spec{Views: []View{clock("c"), rare}, Slow: true}, "")
	waitFor(t, "the first frames", func() bool { return c.has(`<p id="note">0</p>`) })
	for i := range int64(3) {
		// Just after a frame of the clock, which the next waits 250 ms for.
		n := len(clockFrames(c, "c"))
		waitFor(t, "a frame of the clock", func() bool { return len(clockFrames(c, "c")) > n })
		note.Store(i + 1)
		h.Changed([]string{"note"})
		changed := time.Now()
		want := `<p id="note">` + strconv.FormatInt(i+1, 10) + `</p>`
		waitFor(t, "the note", func() bool { return c.has(want) })
		if d := time.Since(changed); d > 150*time.Millisecond {
			t.Errorf("the note took %v", d)
		}
		ps := c.patches()
		k := len(ps) - 1
		for ps[k] != want {
			k--
		}
		if k == 0 || !strings.HasPrefix(ps[k-1], `<p id="c">`) {
			t.Fatalf("the note came without the clock's frame: %q", ps)
		}
	}
}

// A slow stream's heartbeats go out on time while its frames wait.
func TestSlowStreamHeartbeats(t *testing.T) {
	h := start(t, Options{Tick: 10 * time.Millisecond, Heartbeat: 40 * time.Millisecond, HeartbeatSignal: "_ping"})
	c := serve(h, Spec{Views: []View{clock("c")}, Slow: true}, "")
	time.Sleep(time.Second)
	pings := 0
	for _, e := range c.events() {
		if e.name == "datastar-patch-signals" {
			pings++
		}
	}
	if frames := len(clockFrames(c, "c")); pings < 15 || frames > 6 {
		t.Fatalf("in a second, %d heartbeats and %d frames", pings, frames)
	}
}

// Serve opens a slow stream with sse.Options.Slow: with room in the
// budget, zstd at its default level.
func TestSlowStreamDenserEncoder(t *testing.T) {
	h := start(t, Options{SSE: sse.Options{Budget: sse.NewBudget(sse.BudgetOptions{Limit: 1 << 30})}})
	v := text("v", nil, func(context.Context, *sql.Tx) (string, error) { return strings.Repeat("row ", 1000), nil })
	serve(h, Spec{Views: []View{v}, Slow: true}, "zstd")
	serve(h, Spec{Views: []View{v}}, "zstd")
	waitFor(t, "both streams' tiers", func() bool {
		st := h.Budget().Stats().Streams
		return st["zstd default 2^19"] == 1 && st["zstd 2^19"] == 1
	})
}
