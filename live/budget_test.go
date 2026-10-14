package live

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/zweiundeins/lith/sse"
)

// The hub's streams share the budget of Options.SSE: each holds a tier
// while it is open, a stream without room still gets its frames, and every
// stream gives its tier back when it ends.
func TestBudgetThroughOptions(t *testing.T) {
	b := sse.NewBudget(sse.BudgetOptions{Limit: 4 << 20, MinWindow: 17, MaxWindow: 19})
	h := start(t, Options{SSE: sse.Options{Budget: b}})
	v := text("a", nil, func(context.Context, *sql.Tx) (string, error) { return "a", nil })
	var cs []*client
	for range 3 {
		c := serve(h, Spec{Views: []View{v}}, sse.Zstd)
		waitFor(t, "the first frame", func() bool { return strings.Contains(string(unzstd(t, c.bytes())), `<p id="a">a</p>`) })
		cs = append(cs, c)
	}
	// Shares of 4 MiB, 2 MiB and 1.3 MiB, the last with 0.7 MiB left.
	if got := b.Stats().Streams; got["zstd 2^19"] != 2 || got["zstd asleep"] != 1 {
		t.Fatalf("streams by tier: %v", got)
	}
	for _, c := range cs {
		c.leave(t)
	}
	if st := b.Stats(); st.Reserved != 0 || len(st.Streams) != 0 {
		t.Fatalf("after the streams left: %+v", st)
	}

	// With Refuse, a stream without room is answered 503.
	b = sse.NewBudget(sse.BudgetOptions{Limit: 2 << 20, Refuse: true})
	h = start(t, Options{SSE: sse.Options{Budget: b}})
	first := serve(h, Spec{Views: []View{v}}, sse.Zstd)
	waitFor(t, "the first frame", func() bool { return len(unzstd(t, first.bytes())) > 0 })
	second := serve(h, Spec{Views: []View{v}}, sse.Brotli)
	if err := second.returned(t); !errors.Is(err, sse.ErrNoRoom) || second.status() != http.StatusServiceUnavailable {
		t.Fatalf("a stream without room: %v, status %d", err, second.status())
	}
	if st := b.Stats(); st.Refused != 1 || h.Stats().Streams != 1 {
		t.Fatalf("after the refusal: %+v, %d streams", st, h.Stats().Streams)
	}
	first.leave(t)
}

// A stream that sends no frame for Options.Hibernate hibernates and gives
// its tier back; heartbeats leave it asleep, and its next frame wakes it.
// The body decodes to everything it sent.
func TestHibernateInHub(t *testing.T) {
	b := sse.NewBudget(sse.BudgetOptions{Limit: -1})
	h := start(t, Options{Tick: 2 * time.Millisecond, Heartbeat: 15 * time.Millisecond, Hibernate: 40 * time.Millisecond, SSE: sse.Options{Budget: b}})
	var n atomic.Int64
	v := text("n", []string{"n"}, func(context.Context, *sql.Tx) (string, error) { return strconv.Itoa(int(n.Load())), nil })
	c := serve(h, Spec{Views: []View{v}}, sse.Zstd)
	has := func(html string) func() bool {
		return func() bool { return strings.Contains(string(unzstd(t, c.bytes())), html) }
	}
	asleep := func() bool { return b.Stats().Streams["zstd asleep"] == 1 }
	waitFor(t, "the first frame", has(`<p id="n">0</p>`))
	magic := []byte{0x28, 0xb5, 0x2f, 0xfd}
	for i := 1; i <= 2; i++ {
		waitFor(t, "hibernation", asleep)
		body := c.bytes()
		beats, frames := strings.Count(string(unzstd(t, body)), ":\n\n"), bytes.Count(body, magic)
		waitFor(t, "two heartbeats", func() bool { return strings.Count(string(unzstd(t, c.bytes())), ":\n\n") >= beats+2 })
		// Each went out in a zstd frame of its own, after which the stream
		// sleeps again: had a heartbeat woken it, it would stay awake.
		if got := bytes.Count(c.bytes(), magic); got < frames+2 {
			t.Fatalf("%d zstd frames after two heartbeats, from %d", got, frames)
		}
		waitFor(t, "sleep after the heartbeats", asleep)
		n.Store(int64(i))
		h.Changed([]string{"n"})
		waitFor(t, "the next frame", has(`<p id="n">`+strconv.Itoa(i)+`</p>`))
	}
	c.leave(t)
	if st := b.Stats(); len(st.Streams) != 0 {
		t.Fatalf("after the stream left: %+v", st)
	}
}

// A hub without a budget of its own makes one, which a process without a
// memory limit leaves without one.
func TestHubMakesABudget(t *testing.T) {
	h := New(Options{})
	if h.Budget() == nil {
		t.Fatal("no budget")
	}
	b := sse.NewBudget(sse.BudgetOptions{Limit: 1 << 30})
	if h := New(Options{SSE: sse.Options{Budget: b}}); h.Budget() != b {
		t.Fatal("the hub replaced the budget it was given")
	}
}

// sized is a frame of another type than []byte, with a length.
type sized struct{ n int }

func (s sized) Len() int { return s.n }

// The hub books each view's newest frame in the budget once, however many
// streams watch it: its length and 8 KiB. A frame that changes size moves
// the booking, a frame of another type is booked by its Len method, and a
// view nobody watches any more gives its booking back.
func TestHubBooksFrames(t *testing.T) {
	b := sse.NewBudget(sse.BudgetOptions{Limit: 64 << 20})
	h := start(t, Options{Tick: time.Millisecond, SSE: sse.Options{Budget: b}})
	var size atomic.Int64
	size.Store(1000)
	shared := View{Key: "shared", Topics: []string{"shared"}, Render: func(context.Context, *sql.Tx) (any, error) {
		return sse.PatchElements(bytes.Repeat([]byte("x"), int(size.Load()))), nil
	}}
	frameLen := func() int64 { return int64(len(sse.PatchElements(bytes.Repeat([]byte("x"), int(size.Load()))))) }
	frames := func() int64 { return b.Stats().Frames }
	var cs []*client
	for i := range 3 {
		own := View{Key: "own" + strconv.Itoa(i), Render: func(context.Context, *sql.Tx) (any, error) { return sized{5000}, nil }}
		cs = append(cs, serve(h, Spec{Views: []View{shared, own}, WriteTick: func(*sse.Stream, []Frame) error { return nil }}, ""))
	}
	want := frameLen() + frameSlack + 3*(5000+frameSlack)
	waitFor(t, "the frames booked", func() bool { return frames() == want })
	size.Store(200 << 10)
	h.Changed([]string{"shared"})
	want = frameLen() + frameSlack + 3*(5000+frameSlack)
	waitFor(t, "the larger frame booked", func() bool { return frames() == want })
	cs[0].leave(t)
	want -= 5000 + frameSlack
	waitFor(t, "a view left", func() bool { return frames() == want })
	for _, c := range cs[1:] {
		c.leave(t)
	}
	if st := b.Stats(); st.Frames != 0 || st.Reserved != 0 {
		t.Fatalf("after the streams left: %+v", st)
	}
}

// tabView is a tab's own view: a table of rows rows, about 175 bytes each,
// whose cells change with version.
func tabView(key string, rows int, version *atomic.Int64) View {
	return View{Key: key, Topics: []string{"tabs"}, Render: func(context.Context, *sql.Tx) (any, error) {
		v := version.Load()
		var buf bytes.Buffer
		for r := range rows {
			fmt.Fprintf(&buf, `<tr id="row-%d"><td class="name">Item %d</td><td class="value">%d</td><td><button data-on:click="@post('/cmd/inc?id=%d')">+</button></td></tr>`+"\n", r, r, int64(r*7)+v*int64(r%3), r)
		}
		return sse.PatchElements(buf.Bytes()), nil
	}}
}

func liveHeap() int64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc + m.StackInuse)
}

// What the budget books for streams that each watch a view of their own,
// their encoders, their frames and 32 KiB each, is what they hold on the
// heap and the stacks after a collection, within a tenth or 64 KiB a
// stream. (The 32 KiB include net/http's buffers for the connection,
// which these streams have none of.)
func TestBudgetMatchesHeap(t *testing.T) {
	if testing.Short() {
		t.Skip("streams large frames to many streams")
	}
	streams := 12
	if raceEnabled {
		streams = 4
	}
	for _, rows := range []int{900, 1500} {
		for _, coding := range []string{sse.Brotli, sse.Zstd, "identity"} {
			b := sse.NewBudget(sse.BudgetOptions{Limit: 1 << 30})
			h := start(t, Options{Tick: time.Millisecond, Heartbeat: time.Hour, SSE: sse.Options{Budget: b}})
			before := liveHeap()
			var version atomic.Int64
			var cs []*client
			for i := range streams {
				cs = append(cs, serve(h, Spec{Views: []View{tabView("tab"+strconv.Itoa(i), rows, &version)}}, coding))
			}
			for k := range 3 {
				version.Store(int64(k))
				h.Changed([]string{"tabs"})
				waitFor(t, "a frame to every stream", func() bool { return h.Stats().FramesSent >= int64(streams*(k+1)) })
			}
			// The streams' clients keep the bodies: count what the hub holds.
			var bodies int64
			for _, c := range cs {
				c.mu.Lock()
				bodies += int64(c.body.Cap())
				c.mu.Unlock()
			}
			measured := liveHeap() - before - bodies
			st := b.Stats()
			ratio := float64(measured) / float64(st.Reserved)
			t.Logf("%d rows, %s: %d KiB booked (%d KiB of frames), %d KiB measured, %.2f, %v",
				rows, coding, st.Reserved>>10, st.Frames>>10, measured>>10, ratio, st.Streams)
			if d := measured - st.Reserved; max(d, -d) > max(st.Reserved/10, int64(streams)*64<<10) {
				t.Errorf("%d rows, %s: measured %d KiB for %d KiB booked", rows, coding, measured>>10, st.Reserved>>10)
			}
			for _, c := range cs {
				c.leave(t)
			}
		}
	}
}

// A stream with Spec.Reconnect whose budget has room for a better tier
// than it holds ends with sse.Reconnect, so that the page opens it again:
// a brotli stream that took 2^16 for a frame that needs 2^18. A stream
// with the same ID is not asked again within a minute, nor one without an
// ID in its first minute.
func TestReconnectForABetterTier(t *testing.T) {
	b := sse.NewBudget(sse.BudgetOptions{Limit: 8 << 20})
	h := start(t, Options{Tick: time.Millisecond, Heartbeat: time.Hour, SSE: sse.Options{Budget: b}})
	var version atomic.Int64
	view := tabView("tab", 1300, &version)
	f, _ := view.Render(t.Context(), nil)
	booked := int64(len(f.([]byte))) + frameSlack
	// What is left after the stream's view and its 2^16 at quality 3 (752
	// KiB with the stream's own): 200 KiB, too little for 2^18.
	fill := int64(8<<20) - 752<<10 - booked - 200<<10
	below := func(id string) *client {
		t.Helper()
		b.Hold(fill)
		v := view
		v.Key += id
		c := serve(h, Spec{ID: id, Views: []View{v}, Reconnect: "stream"}, sse.Brotli)
		waitFor(t, "the first frame", func() bool { return b.Stats().Streams["br q3 2^16"] == 1 })
		return c
	}
	c := below("tab1")
	b.Hold(-fill)
	if err := c.returned(t); err != nil {
		t.Fatalf("Serve returned %v", err)
	}
	body, err := io.ReadAll(brotli.NewReader(bytes.NewReader(c.bytes())))
	if err != nil || !bytes.HasSuffix(body, sse.Reconnect("stream")) {
		t.Fatalf("the stream ended without Reconnect: %v, %q", err, body[max(0, len(body)-200):])
	}
	if st := b.Stats(); st.Reopened != 1 {
		t.Fatalf("reopened %d", st.Reopened)
	}
	// The same tab, below again: not asked within a minute; nor a stream
	// without an ID.
	for _, id := range []string{"tab1", ""} {
		c = below(id)
		b.Hold(-fill)
		time.Sleep(50 * time.Millisecond)
		select {
		case err := <-c.done:
			t.Fatalf("the stream %q ended: %v", id, err)
		default:
		}
		c.leave(t)
	}
	if st := b.Stats(); st.Reopened != 1 {
		t.Fatalf("reopened %d", st.Reopened)
	}
}
