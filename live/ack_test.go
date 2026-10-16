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
