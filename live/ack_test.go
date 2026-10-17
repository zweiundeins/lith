package live

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sse"
)

// ackEvent is a test's acknowledgement event: <a id=ack>token n</a>.
func ackEvent(token string, n int64) []byte {
	return sse.PatchElements([]byte(`<a id="ack">` + token + " " + strconv.FormatInt(n, 10) + `</a>`))
}

// acks are the tokens and numbers of a stream's acknowledgement events.
func acks(c *client) (token string, ns []int64) {
	for _, p := range c.patches() {
		if v, ok := strings.CutPrefix(p, `<a id="ack">`); ok {
			tok, num, _ := strings.Cut(strings.TrimSuffix(v, "</a>"), " ")
			n, _ := strconv.ParseInt(num, 10, 64)
			token, ns = tok, append(ns, n)
		}
	}
	return token, ns
}

// A stream with a window of one sends a write and waits for its
// acknowledgement before the next; each write ends with its
// acknowledgement event, and while the stream waits, the view it alone
// watches is not rendered.
func TestWindowWaitsForAcks(t *testing.T) {
	h := start(t, Options{Tick: 10 * time.Millisecond})
	c := serve(h, Spec{ID: "tab", Views: []View{clock("c")}, Window: 1, Ack: ackEvent}, "")
	waitFor(t, "the first frame", func() bool { return len(clockFrames(c, "c")) > 0 })
	time.Sleep(300 * time.Millisecond)
	token, ns := acks(c)
	if f := clockFrames(c, "c"); len(f) != 1 || len(ns) != 1 || ns[0] != 1 {
		t.Fatalf("before an acknowledgement: frames %v, acks %v", f, ns)
	}
	ps := c.patches()
	if !strings.HasPrefix(ps[len(ps)-1], `<a id="ack">`) {
		t.Fatalf("the write does not end with its acknowledgement: %q", ps)
	}
	renders := h.Stats().Renders
	time.Sleep(100 * time.Millisecond)
	if d := h.Stats().Renders - renders; d > 1 {
		t.Errorf("%d renders while the stream waited", d)
	}
	if h.Ack("tab", "nope", 1) || h.Ack("other", token, 1) {
		t.Error("an acknowledgement with another token or ID was taken")
	}
	for n := int64(1); n <= 3; n++ {
		before := clockFrames(c, "c")
		if !h.Ack("tab", token, n) {
			t.Fatal("the acknowledgement was not taken")
		}
		waitFor(t, "the next frame", func() bool { return len(clockFrames(c, "c")) > len(before) })
		time.Sleep(100 * time.Millisecond)
		if f := clockFrames(c, "c"); len(f) != len(before)+1 {
			t.Fatalf("after acknowledging %d: frames %v", n, f)
		}
	}
	if _, ns := acks(c); len(ns) != 4 || ns[3] != 4 {
		t.Fatalf("acks %v", ns)
	}
	// An old acknowledgement frees nothing.
	h.Ack("tab", token, 2)
	time.Sleep(100 * time.Millisecond)
	if f := clockFrames(c, "c"); len(f) != 4 {
		t.Fatalf("an old acknowledgement freed the window: %v", f)
	}
}

// When an acknowledgement frees the window, the stream does not send the
// frame that waited for it while its view's render was deferred: the next
// tick renders the view again, and that frame goes out.
func TestWindowSendsAFreshFrame(t *testing.T) {
	h := start(t, Options{Tick: 10 * time.Millisecond})
	c := serve(h, Spec{ID: "tab", Views: []View{clock("c")}, Window: 1, Ack: ackEvent}, "")
	waitFor(t, "the first frame", func() bool { return len(clockFrames(c, "c")) > 0 })
	time.Sleep(200 * time.Millisecond) // a second frame waits, and its view is deferred
	token, _ := acks(c)
	h.Ack("tab", token, 1)
	waitFor(t, "the next frame", func() bool { return len(clockFrames(c, "c")) > 1 })
	f := clockFrames(c, "c")
	if f[1] < f[0]+2 {
		t.Fatalf("the frame that waited went out: %v", f)
	}
	if h.Stats().FramesDropped == 0 {
		t.Error("no frame dropped")
	}
}

// A window of two lets two writes wait for their acknowledgements.
func TestWindowOfTwo(t *testing.T) {
	h := start(t, Options{Tick: 10 * time.Millisecond})
	c := serve(h, Spec{ID: "tab", Views: []View{clock("c")}, Window: 2, Ack: ackEvent}, "")
	waitFor(t, "two frames", func() bool { return len(clockFrames(c, "c")) >= 2 })
	time.Sleep(200 * time.Millisecond)
	token, ns := acks(c)
	if len(ns) != 2 {
		t.Fatalf("acks %v before an acknowledgement", ns)
	}
	h.Ack("tab", token, 2)
	waitFor(t, "two more frames", func() bool { return len(clockFrames(c, "c")) >= 4 })
	time.Sleep(200 * time.Millisecond)
	if f := clockFrames(c, "c"); len(f) != 4 {
		t.Fatalf("frames %v", f)
	}
}

// autoAck acknowledges the newest write of c's stream delay after it
// sees it, as a page would, until the test ends.
func autoAck(t *testing.T, h *Hub, c *client, id string, delay time.Duration) {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		var last int64
		for {
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond):
			}
			token, ns := acks(c)
			if len(ns) == 0 || ns[len(ns)-1] <= last {
				continue
			}
			last = ns[len(ns)-1]
			time.Sleep(delay)
			h.Ack(id, token, last)
		}
	}()
}

// A stream with Ack and no Window sends every frame without asking for
// acknowledgements until Hub.Pace paces it, and again once Pace sets its
// window to 0.
func TestPaceAStreamLater(t *testing.T) {
	h := start(t, Options{Tick: 10 * time.Millisecond})
	c := serve(h, Spec{ID: "tab", Views: []View{clock("c")}, Ack: ackEvent}, "")
	waitFor(t, "five frames", func() bool { return len(clockFrames(c, "c")) >= 5 })
	if _, ns := acks(c); len(ns) > 0 {
		t.Fatalf("an unpaced stream asked for acknowledgements: %v", ns)
	}
	if h.Pace("other", 1) || h.Ack("tab", "x", 1) {
		t.Error("Pace or Ack took a stream that is not open")
	}
	if !h.Pace("tab", 1) || !h.Pace("tab", 1) {
		t.Fatal("Pace did not find the stream")
	}
	if p := h.Stats().Paces; p != 1 {
		t.Errorf("Stats.Paces = %d, want 1 for two calls", p)
	}
	waitFor(t, "an acknowledgement event", func() bool { _, ns := acks(c); return len(ns) == 1 })
	time.Sleep(100 * time.Millisecond)
	before := clockFrames(c, "c")
	time.Sleep(100 * time.Millisecond)
	if f := clockFrames(c, "c"); len(f) != len(before) {
		t.Fatalf("frames went out while the window was full: %d, then %d", len(before), len(f))
	}
	token, ns := acks(c)
	if ns[0] != 1 {
		t.Fatalf("the first paced write is %d, want 1", ns[0])
	}
	h.Ack("tab", token, 1)
	waitFor(t, "the next frame", func() bool { return len(clockFrames(c, "c")) > len(before) })

	h.Pace("tab", 0)
	n := len(clockFrames(c, "c"))
	waitFor(t, "frames without acknowledgements", func() bool { return len(clockFrames(c, "c")) >= n+5 })
	if _, ns := acks(c); len(ns) > 2 {
		t.Errorf("acknowledgements asked for after Pace(0): %v", ns)
	}
	plain := serve(h, Spec{ID: "plain", Views: []View{clock("p")}}, "")
	waitFor(t, "a frame", func() bool { return len(clockFrames(plain, "p")) > 0 })
	if h.Pace("plain", 1) {
		t.Error("Pace paced a stream without Spec.Ack")
	}
}

// A paced stream whose page acknowledges each write before the next frame
// is ready stops pacing after Release, and sends frames without asking
// for acknowledgements from then on.
func TestReleaseWhenThePageKeepsUp(t *testing.T) {
	h := start(t, Options{Tick: 10 * time.Millisecond})
	opened := time.Now()
	c := serve(h, Spec{ID: "tab", Views: []View{clock("c")}, Window: 1, Ack: ackEvent, Release: 150 * time.Millisecond}, "")
	autoAck(t, h, c, "tab", 0)
	waitFor(t, "the release", func() bool { return h.Stats().Releases == 1 })
	if d := time.Since(opened); d < 150*time.Millisecond {
		t.Errorf("released after %v", d)
	}
	time.Sleep(20 * time.Millisecond)
	_, before := acks(c)
	n := len(clockFrames(c, "c"))
	waitFor(t, "ten more frames", func() bool { return len(clockFrames(c, "c")) >= n+10 })
	if _, ns := acks(c); len(ns) != len(before) {
		t.Errorf("acknowledgements asked for after the release: %d, then %d", len(before), len(ns))
	}
}

// A page that acknowledges each write after the next frame is ready
// stays paced.
func TestNoReleaseWhileThePageIsLate(t *testing.T) {
	h := start(t, Options{Tick: 10 * time.Millisecond})
	c := serve(h, Spec{ID: "tab", Views: []View{clock("c")}, Window: 1, Ack: ackEvent, Release: 100 * time.Millisecond}, "")
	autoAck(t, h, c, "tab", 40*time.Millisecond)
	time.Sleep(700 * time.Millisecond)
	if r := h.Stats().Releases; r != 0 {
		t.Fatalf("%d releases of a page that was late", r)
	}
	if _, ns := acks(c); len(ns) < 5 {
		t.Fatalf("only %d acknowledgements", len(ns))
	}
}

// Each release of the same stream doubles the time until the next.
func TestReleaseBacksOff(t *testing.T) {
	h := start(t, Options{Tick: 10 * time.Millisecond})
	c := serve(h, Spec{ID: "tab", Views: []View{clock("c")}, Window: 1, Ack: ackEvent, Release: 100 * time.Millisecond}, "")
	autoAck(t, h, c, "tab", 0)
	waitFor(t, "the first release", func() bool { return h.Stats().Releases == 1 })
	paced := time.Now()
	h.Pace("tab", 1)
	waitFor(t, "the second release", func() bool { return h.Stats().Releases == 2 })
	if d := time.Since(paced); d < 200*time.Millisecond {
		t.Errorf("the second release came %v after pacing again, want 200 ms or more", d)
	}
}
