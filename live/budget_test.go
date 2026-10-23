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
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/zweiundeins/lith/sse"
)

// The hub's streams share the budget of Options.SSE: each holds a tier
// while it is open, a stream without room still gets its frames, and every
// stream gives its tier back when it ends. With Refuse, a stream without
// room gets a 503, an uncompressed one too.
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
	// So is a stream that compresses nothing, once its 32 KiB do not fit.
	st := b.Stats()
	b.Hold(st.Limit - st.Reserved - 16<<10)
	plain := serve(h, Spec{Views: []View{v}}, "identity")
	if err := plain.returned(t); !errors.Is(err, sse.ErrNoRoom) || plain.status() != http.StatusServiceUnavailable {
		t.Fatalf("an uncompressed stream without room: %v, status %d", err, plain.status())
	}
	if st := b.Stats(); st.Refused != 2 || h.Stats().Streams != 1 {
		t.Fatalf("after the second refusal: %+v, %d streams", st, h.Stats().Streams)
	}
	first.leave(t)
}

// A stream that sends no frame for Options.Hibernate hibernates and gives
// its tier back; heartbeats go out without waking it, and its next frame
// wakes it. The body decodes to everything it sent.
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
	// A heartbeat in a zstd frame of raw blocks: a single segment of 3
	// bytes, in one last raw block of 3. An encoder would have compressed it
	// in a frame with a window.
	raw := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x20, 3, 3<<3 | 1, 0, 0, ':', '\n', '\n'}
	for i := 1; i <= 2; i++ {
		waitFor(t, "hibernation", asleep)
		body := c.bytes()
		beats, frames := strings.Count(string(unzstd(t, body)), ":\n\n"), bytes.Count(body, raw)
		waitFor(t, "two heartbeats", func() bool { return strings.Count(string(unzstd(t, c.bytes())), ":\n\n") >= beats+2 })
		if got := bytes.Count(c.bytes(), raw); got < frames+2 {
			t.Fatalf("%d raw heartbeats after two heartbeats, from %d", got, frames)
		}
		if !asleep() {
			t.Fatal("a heartbeat woke the stream")
		}
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

// The hub books each frame it keeps in the budget once, however many
// streams watch it: its length and 8 KiB. It keeps the newest frame of a
// view that two or more streams watch, and of one whose frame is not
// []byte, booked by its Len method; of a []byte view that one stream
// watches, none. A frame that changes size moves the booking, and a view
// that falls to one stream, or to none, gives it back.
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
		tab := View{Key: "tab" + strconv.Itoa(i), Render: func(context.Context, *sql.Tx) (any, error) {
			return sse.PatchElements(bytes.Repeat([]byte("t"), 100<<10)), nil
		}}
		cs = append(cs, serve(h, Spec{Views: []View{shared, own, tab}, WriteTick: func(*sse.Stream, []Frame) error { return nil }}, ""))
	}
	want := frameLen() + frameSlack + 3*(5000+frameSlack)
	waitFor(t, "the frames booked", func() bool { return frames() == want && h.Stats().FramesSent == 9 })
	size.Store(200 << 10)
	h.Changed([]string{"shared"})
	want = frameLen() + frameSlack + 3*(5000+frameSlack)
	waitFor(t, "the larger frame booked", func() bool { return frames() == want })
	cs[0].leave(t)
	want -= 5000 + frameSlack
	waitFor(t, "a view left", func() bool { return frames() == want })
	cs[1].leave(t)
	want -= 5000 + frameSlack + frameLen() + frameSlack
	waitFor(t, "the shared view down to one stream", func() bool { return frames() == want })
	cs[2].leave(t)
	if st := b.Stats(); st.Frames != 0 || st.Reserved != 0 {
		t.Fatalf("after the streams left: %+v", st)
	}
}

// A view whose last stream leaves while it renders keeps and books no
// frame from that render, even one that is not []byte.
func TestViewLeftDuringItsRenderBooksNothing(t *testing.T) {
	b := sse.NewBudget(sse.BudgetOptions{Limit: 64 << 20})
	h := start(t, Options{Tick: time.Millisecond, SSE: sse.Options{Budget: b}})
	entered, release := make(chan struct{}), make(chan struct{})
	v := View{Key: "v", Render: func(context.Context, *sql.Tx) (any, error) {
		close(entered)
		<-release
		return sized{5000}, nil
	}}
	c := serve(h, Spec{Views: []View{v}, WriteTick: func(*sse.Stream, []Frame) error { return nil }}, "")
	<-entered
	c.leave(t)
	close(release)
	// The probe's frame comes in a later tick than that render's.
	p := text("probe", nil, func(context.Context, *sql.Tx) (string, error) { return "p", nil })
	probe := serve(h, Spec{Views: []View{p}}, "")
	waitFor(t, "the probe", func() bool { return probe.has(`<p id="probe">p</p>`) })
	if st := b.Stats(); st.Frames != 0 {
		t.Fatalf("%d bytes of frames booked for a view nobody watches", st.Frames)
	}
	probe.leave(t)
}

// With Refuse, what the budget books stays within its limit while tabs
// with a 246 KB view of their own open until one is turned away: the hub
// books no frame of a view that one stream watches, so no booking comes
// after Open has let a stream in.
func TestTabsStayWithinARefusingBudget(t *testing.T) {
	for _, coding := range []string{sse.Zstd, sse.Brotli, "identity"} {
		b := sse.NewBudget(sse.BudgetOptions{Limit: 8 << 20, Refuse: true})
		h := start(t, Options{Tick: time.Millisecond, Heartbeat: time.Hour, SSE: sse.Options{Budget: b}})
		var version atomic.Int64
		var cs []*client
		for i := 0; ; i++ {
			if i == 300 {
				t.Fatalf("%s: no stream turned away", coding)
			}
			c := serve(h, Spec{Views: []View{tabView("tab"+strconv.Itoa(i), 1500, &version)}}, coding)
			waitFor(t, "the frame or the refusal", func() bool {
				return h.Stats().FramesSent == int64(len(cs)+1) || c.status() == http.StatusServiceUnavailable
			})
			if c.status() == http.StatusServiceUnavailable {
				break
			}
			cs = append(cs, c)
			if st := b.Stats(); st.Reserved > st.Limit {
				t.Fatalf("%s, %d streams: %d KiB booked in a limit of %d KiB", coding, len(cs), st.Reserved>>10, st.Limit>>10)
			}
		}
		for _, c := range cs {
			c.leave(t)
		}
	}
}

// A view that one stream watches keeps no frame once the stream has it:
// after the write, whatever the coding, the frame's bytes are garbage and
// the budget books none. A second stream that joins the view gets a frame
// rendered in the next tick, which the view keeps, booked once, while
// both watch it; when one leaves, the view drops it.
func TestViewWithOneStreamKeepsNoFrame(t *testing.T) {
	const size = 8 << 20
	frame := int64(len(sse.PatchElements(bytes.Repeat([]byte("x"), size))))
	for _, coding := range []string{"identity", sse.Zstd, sse.Brotli} {
		b := sse.NewBudget(sse.BudgetOptions{Limit: 1 << 30})
		h := start(t, Options{Tick: time.Millisecond, Heartbeat: time.Hour, SSE: sse.Options{Budget: b}})
		v := View{Key: "tab", Render: func(context.Context, *sql.Tx) (any, error) {
			return sse.PatchElements(bytes.Repeat([]byte("x"), size)), nil
		}}
		// What the hub holds: the heap and the stacks, less the bodies the
		// clients keep and the encoders the budget books.
		before := liveHeap()
		var cs []*client
		held := func() int64 {
			var bodies int64
			for _, c := range cs {
				c.mu.Lock()
				bodies += int64(c.body.Cap())
				c.mu.Unlock()
			}
			st := b.Stats()
			return liveHeap() - before - bodies - (st.Reserved - st.Frames)
		}
		cs = append(cs, serve(h, Spec{Views: []View{v}}, coding))
		waitFor(t, "the frame", func() bool { return h.Stats().FramesSent == 1 })
		one := held()
		if one > size/8 {
			t.Errorf("%s: one stream: the hub holds %d KiB after the frame went out", coding, one>>10)
		}
		if st := b.Stats(); st.Frames != 0 {
			t.Errorf("%s: one stream: %d bytes of frames booked", coding, st.Frames)
		}
		cs = append(cs, serve(h, Spec{Views: []View{v}}, coding))
		waitFor(t, "the second stream's frame", func() bool { return h.Stats().FramesSent == 2 })
		two := held()
		if two < size*7/8 || two > size+size/8 {
			t.Errorf("%s: two streams: the hub holds %d KiB, the frame is %d KiB", coding, two>>10, frame>>10)
		}
		if st, r := b.Stats(), h.Stats(); st.Frames != frame+frameSlack || r.Renders != 2 {
			t.Errorf("%s: two streams: %d bytes of frames booked, %d renders", coding, st.Frames, r.Renders)
		}
		cs[1].leave(t)
		again := held()
		if again > size/8 {
			t.Errorf("%s: one stream again: the hub holds %d KiB", coding, again>>10)
		}
		t.Logf("%s, a frame of %d KiB: the hub holds %d KiB with one stream, %d with two, %d with one again",
			coding, frame>>10, one>>10, two>>10, again>>10)
		if st := b.Stats(); st.Frames != 0 {
			t.Errorf("%s: one stream again: %d bytes of frames booked", coding, st.Frames)
		}
		cs[0].leave(t)
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
// or one view for every two, is what they hold on the heap and the stacks
// after a collection, within a tenth or 64 KiB a stream: their encoders,
// 32 KiB each, and the frames of the views two streams watch. (The 32 KiB
// include net/http's buffers for the connection, which these streams have
// none of.)
func TestBudgetMatchesHeap(t *testing.T) {
	if testing.Short() {
		t.Skip("streams large frames to many streams")
	}
	streams := 12
	if raceEnabled {
		streams = 4
	}
	for _, rows := range []int{900, 1500} {
		for _, per := range []int{1, 2} {
			for _, coding := range []string{sse.Brotli, sse.Zstd, "identity"} {
				matchHeap(t, rows, streams, per, coding)
			}
		}
	}
}

// matchHeap opens streams streams, per of them on each view, and compares
// what the budget books with what the heap and the stacks hold.
func matchHeap(t *testing.T, rows, streams, per int, coding string) {
	t.Helper()
	b := sse.NewBudget(sse.BudgetOptions{Limit: 1 << 30})
	h := start(t, Options{Tick: time.Millisecond, Heartbeat: time.Hour, SSE: sse.Options{Budget: b}})
	before := liveHeap()
	var version atomic.Int64
	var cs []*client
	for i := range streams {
		cs = append(cs, serve(h, Spec{Views: []View{tabView("tab"+strconv.Itoa(i/per), rows, &version)}}, coding))
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
	t.Logf("%d rows, %d streams a view, %s: %d KiB booked (%d KiB of frames), %d KiB measured, %.2f, %v",
		rows, per, coding, st.Reserved>>10, st.Frames>>10, measured>>10, ratio, st.Streams)
	if d := measured - st.Reserved; max(d, -d) > max(st.Reserved/10, int64(streams)*64<<10) {
		t.Errorf("%d rows, %d streams a view, %s: measured %d KiB for %d KiB booked", rows, per, coding, measured>>10, st.Reserved>>10)
	}
	for _, c := range cs {
		c.leave(t)
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
	// What is left after the stream's 2^16 at quality 3 (752 KiB with the
	// stream's own): 200 KiB, too little for 2^18. The hub keeps no frame of
	// a view that one stream watches.
	fill := int64(8<<20) - 752<<10 - 200<<10
	below := func(id string) *client {
		t.Helper()
		b.Hold(fill)
		v := view
		v.Key += id
		c := serve(h, Spec{ID: id, Views: []View{v}, Reconnect: "stream"}, sse.Brotli)
		// A brotli stream holds q3 2^16 from Open: the frame on the wire
		// shows that its first write picked it.
		waitFor(t, "the first frame", func() bool { return len(c.bytes()) > 0 && b.Stats().Streams["br q3 2^16"] == 1 })
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

// With Refuse, Serve opens a stream once its first frame is there and lets
// it in only with room for a window that holds that frame: tabs with a
// 230 KB view of their own, in budgets that hold six zstd tabs at 2^18,
// eight brotli tabs at quality 3 (seven get in: the first two take
// quality 4) or four of each (seven). Every tab let in holds a window that
// holds its frame; the next gets a 503 and nothing else, also after the
// zstd tabs moved down to make room, and gets in once a tab of its coding
// leaves.
func TestRefuseNeedsTheFrameWindow(t *testing.T) {
	const zstdNeed, brotliNeed = (880 + 32) << 10, (1104 + 32) << 10
	for _, c := range []struct {
		name    string
		codings []string
		limit   int64
		want    int
	}{
		{"zstd", []string{sse.Zstd}, 6 * zstdNeed, 6},
		{"brotli", []string{sse.Brotli}, 8 * brotliNeed, 7},
		{"mixed", []string{sse.Zstd, sse.Brotli}, 4*zstdNeed + 4*brotliNeed, 7},
	} {
		b := sse.NewBudget(sse.BudgetOptions{Limit: c.limit, Refuse: true})
		h := start(t, Options{Tick: time.Millisecond, Heartbeat: time.Hour, SSE: sse.Options{Budget: b}})
		var version atomic.Int64
		f, _ := tabView("x", 1410, &version).Render(t.Context(), nil)
		frame := len(f.([]byte))
		var cs []*client
		sent := int64(0)
		round := func() {
			t.Helper()
			version.Add(1)
			h.Changed([]string{"tabs"})
			sent += int64(len(cs))
			waitFor(t, "a frame to every tab", func() bool { return h.Stats().FramesSent == sent })
		}
		tab := 0
		try := func(coding string) *client {
			t.Helper()
			tab++
			cl := serve(h, Spec{Views: []View{tabView("tab"+strconv.Itoa(tab), 1410, &version)}}, coding)
			waitFor(t, "the frame or the refusal", func() bool {
				return h.Stats().FramesSent == sent+1 || cl.status() == http.StatusServiceUnavailable
			})
			if cl.status() != http.StatusServiceUnavailable {
				sent++
				return cl
			}
			if err := cl.returned(t); !errors.Is(err, sse.ErrNoRoom) {
				t.Fatalf("%s: a refused tab: %v", c.name, err)
			}
			if e := cl.header.Get("Content-Encoding"); e != "" || len(cl.bytes()) != 0 {
				t.Fatalf("%s: a refused tab got Content-Encoding %q and %q", c.name, e, cl.bytes())
			}
			return nil
		}
		var refused string
		for i := 0; refused == ""; i++ {
			coding := c.codings[i%len(c.codings)]
			cl := try(coding)
			if cl == nil {
				round()
				if cl = try(coding); cl == nil {
					refused = coding
					break
				}
			}
			cs = append(cs, cl)
			round()
		}
		st := b.Stats()
		for k, n := range st.Streams {
			if !holdsFrame(k, frame) {
				t.Errorf("%s: %d tabs at %s, which does not hold a frame of %d bytes", c.name, n, k, frame)
			}
		}
		if len(cs) != c.want || st.Refused < 2 {
			t.Errorf("%s: %d tabs let in, want %d; %d refused: %v", c.name, len(cs), c.want, st.Refused, st.Streams)
		}
		t.Logf("%s, a frame of %d bytes: %d tabs, %v, %d KiB left", c.name, frame, len(cs), st.Streams, (st.Limit-st.Reserved)>>10)
		i := slices.IndexFunc(cs, func(cl *client) bool { return cl.header.Get("Content-Encoding") == refused })
		cs[i].leave(t)
		cs = slices.Delete(cs, i, i+1)
		cl := try(refused)
		if cl == nil {
			t.Fatalf("%s: a %s tab after one left: refused", c.name, refused)
		}
		cs = append(cs, cl)
		for k := range b.Stats().Streams {
			if !holdsFrame(k, frame) {
				t.Errorf("%s: after the retry, a tab at %s", c.name, k)
			}
		}
		for _, cl := range cs {
			cl.leave(t)
		}
	}
}

// holdsFrame reports whether a tier, as the budget's Stats name it, holds

// With Refuse, a refreshed stream needs a window that holds its largest
// write: the Refresh's events go out on their own, before the first
// frames, so a 230 KB container and a 230 KB view need 2^18, not the
// 2^19 that both together would.
func TestRefuseRefreshNeedsTheLargerWrite(t *testing.T) {
	const zstdNeed = (880 + 32) << 10 // zstd 2^18 low memory and a stream
	b := sse.NewBudget(sse.BudgetOptions{Limit: zstdNeed + 64<<10, Refuse: true})
	h := start(t, Options{Tick: time.Millisecond, Heartbeat: time.Hour, Build: "new", SSE: sse.Options{Budget: b}})
	var version atomic.Int64
	view := tabView("tab", 1410, &version)
	f, _ := view.Render(t.Context(), nil)
	lead := f.([]byte)
	c := serve(h, Spec{Views: []View{view}, Build: "old", Refresh: func(context.Context, RefreshRequest) ([][]byte, error) {
		return [][]byte{lead}, nil
	}}, sse.Zstd)
	waitFor(t, "the first frame or the refusal", func() bool {
		return h.Stats().FramesSent == 1 || c.status() == http.StatusServiceUnavailable
	})
	if c.status() == http.StatusServiceUnavailable {
		t.Fatalf("refused, with %v", b.Stats())
	}
	if st := b.Stats(); st.Streams["zstd 2^18 low memory"] != 1 {
		t.Fatalf("a frame and a lead of %d bytes each: %v", len(lead), st.Streams)
	}
	c.leave(t)
}

// a frame of n bytes with a sixteenth to spare.
func holdsFrame(tier string, n int) bool {
	_, w, ok := strings.Cut(tier, "2^")
	if !ok {
		return false
	}
	w, _, _ = strings.Cut(w, " ")
	bits, err := strconv.Atoi(w)
	return err == nil && int64(n)*16 <= int64(15)<<bits
}

// A view that Watch gives a stream while Serve waits for its first frame
// is not lost when the budget then turns the stream away: the next stream
// with its ID watches it.
func TestRefusedStreamKeepsItsWatches(t *testing.T) {
	b := sse.NewBudget(sse.BudgetOptions{Limit: 2 << 20, Refuse: true})
	h := start(t, Options{Tick: time.Millisecond, Heartbeat: time.Hour, SSE: sse.Options{Budget: b}})
	var version atomic.Int64
	// It takes zstd 2^19 and leaves no room for a second tab at 2^18.
	first := serve(h, Spec{Views: []View{tabView("first", 1410, &version)}}, sse.Zstd)
	waitFor(t, "the first tab's frame", func() bool { return h.Stats().FramesSent == 1 })
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	tab := tabView("tab", 1410, &version)
	render := tab.Render
	tab.Render = func(ctx context.Context, tx *sql.Tx) (any, error) {
		once.Do(func() {
			close(entered)
			<-release
		})
		return render(ctx, tx)
	}
	c := serve(h, Spec{ID: "t1", Views: []View{tab}}, sse.Zstd)
	<-entered
	extra := text("extra", nil, func(context.Context, *sql.Tx) (string, error) { return "x", nil })
	if !h.Watch("t1", extra) {
		t.Fatal("Watch found no stream while it waited for its frame")
	}
	close(release)
	if err := c.returned(t); !errors.Is(err, sse.ErrNoRoom) || c.status() != http.StatusServiceUnavailable {
		t.Fatalf("the second tab: %v, status %d", err, c.status())
	}
	first.leave(t)
	again := serve(h, Spec{ID: "t1", Views: []View{tab}}, sse.Zstd)
	waitFor(t, "the watched view on the next stream", func() bool {
		return strings.Contains(string(unzstd(t, again.bytes())), `<p id="extra">x</p>`)
	})
	again.leave(t)
}

// The budget shares its room per stream, so a client that opens many
// streams first would keep the compressed tiers, and a client that comes
// later would get its frames uncompressed. MaxStreamsPerClient stops
// that: one client's streams past its cap get 429, and a later client's
// stream on the same 230 KB view keeps a window that holds the frame,
// and sends 4 to 10 KB for each change, by the tier it gets. Without the cap it finds no room
// and sends the whole frame.
func TestLateClientKeepsCompression(t *testing.T) {
	const zstdNeed = (880 + 32) << 10
	perFrame := func(t *testing.T, perClient int) (bytes int, refused int) {
		var version atomic.Int64
		board := tabView("board", 1410, &version)
		f, _ := board.Render(t.Context(), nil)
		frame := int64(len(f.([]byte)))
		b := sse.NewBudget(sse.BudgetOptions{Limit: frame + 8<<10 + 4*zstdNeed})
		h := start(t, Options{Tick: time.Millisecond, Heartbeat: time.Hour, MaxStreamsPerClient: perClient, SSE: sse.Options{Budget: b}})
		var open []*client
		sent := int64(0)
		round := func() {
			t.Helper()
			version.Add(1)
			h.Changed([]string{"tabs"})
			sent += int64(len(open))
			waitFor(t, "a frame to every stream", func() bool { return h.Stats().FramesSent == sent })
		}
		for range 10 {
			c := serve(h, Spec{Client: "198.51.100.7", Views: []View{board}}, sse.Zstd)
			waitFor(t, "the frame or a refusal", func() bool { return h.Stats().FramesSent == sent+1 || c.status() != 0 })
			if c.status() == http.StatusTooManyRequests {
				refused++
				continue
			}
			sent++
			open = append(open, c)
			round()
		}
		late := serve(h, Spec{Client: "203.0.113.9", Views: []View{board}}, sse.Zstd)
		sent++
		open = append(open, late)
		waitFor(t, "the late client's first frame", func() bool { return h.Stats().FramesSent == sent })
		round()
		before := len(late.bytes())
		round()
		return len(late.bytes()) - before, refused
	}
	n, refused := perFrame(t, 3)
	if refused != 7 || n > 16<<10 {
		t.Errorf("with a cap of 3: %d streams refused, the late client sent %d bytes for a change", refused, n)
	}
	if n, refused := perFrame(t, -1); refused != 0 || n < 64<<10 {
		t.Errorf("without a cap: %d streams refused, the late client sent %d bytes for a change", refused, n)
	}
}
