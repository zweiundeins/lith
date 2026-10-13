package sse

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"

	"github.com/zweiundeins/lith/internal/shutdown"
)

// recorder is a ResponseWriter with write deadlines. A stalled recorder
// takes no bytes, like a client that stopped reading, until its deadline.
type recorder struct {
	header http.Header

	mu       sync.Mutex
	body     bytes.Buffer
	flushes  int
	deadline time.Time
	stalled  bool
	blocked  chan struct{} // closed when a write first blocks
}

func newRecorder() *recorder {
	return &recorder{header: http.Header{}, blocked: make(chan struct{})}
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) WriteHeader(int)     {}

func (r *recorder) Write(p []byte) (int, error) {
	for {
		r.mu.Lock()
		if !r.stalled {
			defer r.mu.Unlock()
			return r.body.Write(p)
		}
		select {
		case <-r.blocked:
		default:
			close(r.blocked)
		}
		d := r.deadline
		r.mu.Unlock()
		if !d.IsZero() && !time.Now().Before(d) {
			return 0, os.ErrDeadlineExceeded
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *recorder) FlushError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushes++
	return nil
}

func (r *recorder) SetWriteDeadline(t time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deadline = t
	return nil
}

func (r *recorder) stall(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stalled = on
}

func (r *recorder) writeDeadline() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deadline
}

func (r *recorder) bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return bytes.Clone(r.body.Bytes())
}

// endedContext is a request context that can end, as Done and Err tell,
// without running the functions given to context.AfterFunc: the moment
// after net/http ends a request, before the stream's abort has run.
type endedContext struct {
	context.Context
	done chan struct{}

	mu  sync.Mutex
	err error
}

func newEndedContext() *endedContext {
	return &endedContext{Context: context.Background(), done: make(chan struct{})}
}

func (c *endedContext) Done() <-chan struct{} { return c.done }

func (c *endedContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *endedContext) AfterFunc(func()) func() bool {
	var once sync.Once
	return func() (stopped bool) {
		once.Do(func() { stopped = true })
		return stopped
	}
}

func (c *endedContext) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = context.Canceled
	close(c.done)
}

func request(ctx context.Context, acceptEncoding string) *http.Request {
	r := httptest.NewRequestWithContext(ctx, "GET", "/", nil)
	if acceptEncoding != "" {
		r.Header.Set("Accept-Encoding", acceptEncoding)
	}
	return r
}

func decoder(t *testing.T, encoding string, body []byte) io.Reader {
	t.Helper()
	switch encoding {
	case Zstd:
		d, err := zstd.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(d.Close)
		return d
	case Brotli:
		return brotli.NewReader(bytes.NewReader(body))
	case Gzip:
		d, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	return bytes.NewReader(body)
}

func TestNegotiate(t *testing.T) {
	for in, want := range map[string]string{
		"gzip, deflate, br, zstd": Zstd,
		"gzip, deflate, br":       Brotli,
		"gzip, br;q=0, zstd;q=0":  Gzip,
		"br;q=0.5, gzip":          Gzip,
		"zstd;q=0.5, br;q=0.5":    Zstd,
		"ZSTD":                    Zstd,
		"br; q=0.8":               Brotli,
		"*":                       Zstd,
		"gzip;q=0.2, *;q=0":       Gzip,
		"br;q=x":                  "",
		"identity":                "",
		"deflate":                 "",
		"":                        "",
	} {
		if got := Negotiate(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	events := [][]byte{
		PatchElements([]byte(`<p id="a">one</p>`)),
		PatchSignals([]byte(`{"n":1}`), false),
		Comment("ping"),
		PatchElements([]byte(strings.Repeat(`<li>row</li>`, 2000)), Patch{Selector: "#l", Mode: Inner}),
	}
	for _, enc := range []string{Zstd, Brotli, Gzip, ""} {
		t.Run("encoding "+enc, func(t *testing.T) {
			w := newRecorder()
			w.Header().Set("Vary", "Origin")
			s, err := Open(w, request(t.Context(), enc), Options{})
			if err != nil {
				t.Fatal(err)
			}
			if s.Encoding() != enc {
				t.Fatalf("encoding %q", s.Encoding())
			}
			var want []byte
			for i, e := range events {
				// The first write sends two events at once.
				if i == 1 {
					continue
				}
				batch := [][]byte{e}
				if i == 0 {
					batch = events[:2]
				}
				if err := s.Write(batch...); err != nil {
					t.Fatal(err)
				}
				for _, b := range batch {
					want = append(want, b...)
				}
				// Each write is flushed: what went out so far decodes to
				// every event so far.
				got := make([]byte, len(want))
				if _, err := io.ReadFull(decoder(t, enc, w.bytes()), got); err != nil {
					t.Fatalf("after write %d: %v", i, err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("after write %d: got %q", i, got)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(decoder(t, enc, w.bytes()))
			if err != nil {
				t.Fatalf("decoding the whole stream: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("got %q", got)
			}
			if err := s.Write(events[0]); !errors.Is(err, ErrClosed) {
				t.Fatalf("write after close: %v", err)
			}
			in, wire := s.Sent()
			if in != int64(len(want)) || wire != int64(len(w.bytes())) {
				t.Fatalf("sent %d, %d; want %d, %d", in, wire, len(want), len(w.bytes()))
			}
			h := w.Header()
			if h.Get("Content-Type") != "text/event-stream" || h.Get("Cache-Control") != "no-store" ||
				h.Get("X-Accel-Buffering") != "no" || h.Get("Content-Encoding") != enc ||
				strings.Join(h.Values("Vary"), ",") != "Origin,Accept-Encoding" {
				t.Fatalf("headers %v", h)
			}
		})
	}
}

// ZstdLevel picks the encoder's level: a higher one sends fewer bytes for
// the same frames, and the stream still decodes to them.
func TestZstdLevel(t *testing.T) {
	sent := map[zstd.EncoderLevel]int64{}
	for _, level := range []zstd.EncoderLevel{0, zstd.SpeedFastest, zstd.SpeedDefault} {
		w := newRecorder()
		s, err := Open(w, request(t.Context(), Zstd), Options{ZstdLevel: level})
		if err != nil {
			t.Fatal(err)
		}
		var want []byte
		for i := range 20 {
			f := frame(i)
			if err := s.Write(f); err != nil {
				t.Fatal(err)
			}
			want = append(want, f...)
		}
		_, sent[level] = s.Sent()
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(decoder(t, Zstd, w.bytes()))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("level %v: the stream decodes to %d bytes of %d, %v", level, len(got), len(want), err)
		}
	}
	if sent[0] != sent[zstd.SpeedFastest] || sent[zstd.SpeedDefault] >= sent[zstd.SpeedFastest] {
		t.Fatalf("bytes on the wire by level: %v", sent)
	}
	if _, err := Open(newRecorder(), request(t.Context(), Zstd), Options{ZstdLevel: 99}); err == nil {
		t.Fatal("opened a stream with an unknown zstd level")
	}
}

func TestWriteTimeout(t *testing.T) {
	w := newRecorder()
	s, err := Open(w, request(t.Context(), "zstd"), Options{WriteTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w.stall(true)
	start := time.Now()
	err = s.Write(PatchElements([]byte(`<p id="a"></p>`)))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("write to a stalled client: %v", err)
	}
	if d := time.Since(start); d < 40*time.Millisecond || d > time.Second {
		t.Fatalf("write failed after %v", d)
	}
	w.stall(false)
	if err2 := s.Write(Comment("")); err2 != err {
		t.Fatalf("a broken stream wrote again: %v", err2)
	}
}

// A stream stalled in its network write does not hold its compression
// slot: another stream compresses and sends meanwhile.
func TestCompressSlotNotHeldDuringNetworkWrite(t *testing.T) {
	slots := make(chan struct{}, 1)
	opt := Options{Compress: slots, WriteTimeout: 5 * time.Second}
	stalled, healthy := newRecorder(), newRecorder()
	a, err := Open(stalled, request(t.Context(), "br"), opt)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(healthy, request(t.Context(), "br"), opt)
	if err != nil {
		t.Fatal(err)
	}
	stalled.stall(true)
	done := make(chan error, 1)
	go func() { done <- a.Write(PatchElements([]byte(`<p id="a"></p>`))) }()
	<-stalled.blocked
	sent := make(chan error, 1)
	go func() { sent <- b.Write(PatchElements([]byte(`<p id="b"></p>`))) }()
	select {
	case err := <-sent:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the healthy stream waited for the stalled one")
	}
	stalled.stall(false)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(slots) != 0 {
		t.Fatal("a slot is still taken")
	}
	a.Close()
	b.Close()
}

// When the request's context ends, a write in progress fails at once,
// long before its deadline, and Close does not wait.
func TestContextEndAbortsWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	w := newRecorder()
	s, err := Open(w, request(ctx, "zstd"), Options{WriteTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	w.stall(true)
	done := make(chan error, 1)
	go func() { done <- s.Write(PatchElements([]byte(`<p id="a"></p>`))) }()
	<-w.blocked
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the write succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the write outlived its stream")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
	if err := s.Write(Comment("")); err == nil {
		t.Fatal("wrote after the stream ended")
	}
}

// leaving is a client that goes away during a write once leave is set:
// like net/http, it ends the request's context before the write fails.
type leaving struct {
	*recorder
	leave func()
}

func (l *leaving) Write(p []byte) (int, error) {
	if l.leave != nil {
		l.leave()
		return 0, errors.New("write: broken pipe")
	}
	return l.recorder.Write(p)
}

// A write that fails because the client left reports the end of the
// stream, not the I/O error, even when the stream's abort has not run.
func TestClientLeavingMidWrite(t *testing.T) {
	for _, enc := range []string{Zstd, ""} {
		ctx := newEndedContext()
		w := &leaving{recorder: newRecorder()}
		s, err := Open(w, request(ctx, enc), Options{})
		if err != nil {
			t.Fatal(err)
		}
		w.leave = ctx.end
		if err := s.Write(Comment("x")); !errors.Is(err, context.Canceled) {
			t.Fatalf("encoding %q: the write failed with %v", enc, err)
		}
		s.Close()
	}
}

// Close sends no end frame once the request is over, even when the
// stream's abort has not run yet.
func TestCloseAfterContextEnd(t *testing.T) {
	ctx := newEndedContext()
	w := newRecorder()
	s, err := Open(w, request(ctx, "zstd"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(Comment("x")); err != nil {
		t.Fatal(err)
	}
	sent := len(w.bytes())
	ctx.end()
	s.Close()
	if n := len(w.bytes()) - sent; n > 0 {
		t.Fatalf("Close wrote %d bytes after the request ended", n)
	}
}

// When the request's context ends during Close's end frame, that write
// fails at once like any other.
func TestContextEndAbortsClose(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	w := newRecorder()
	s, err := Open(w, request(ctx, "zstd"), Options{WriteTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(Comment("x")); err != nil {
		t.Fatal(err)
	}
	w.stall(true)
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	<-w.blocked
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Close returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close outlived its stream")
	}
}

// When the hub stops (shutdown.Cause) while the stream is idle, the
// response stays whole: no write deadline is left behind, later events
// are refused, and Close still ends the compressed stream.
func TestShutdownEndsStreamCleanly(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	w := newRecorder()
	s, err := Open(w, request(ctx, Zstd), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(Comment("x")); err != nil {
		t.Fatal(err)
	}
	cancel(shutdown.Cause)
	s.abort()
	if err := s.Write(Comment("y")); !errors.Is(err, shutdown.Cause) {
		t.Fatalf("a write after the shutdown: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if d := w.writeDeadline(); !d.IsZero() {
		t.Fatalf("left a write deadline of %v", d)
	}
	got, err := io.ReadAll(decoder(t, Zstd, w.bytes()))
	if err != nil || string(got) != ": x\n\n" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// NoCompression sends the events as they are, whatever the client accepts.
func TestNoCompression(t *testing.T) {
	w := newRecorder()
	s, err := Open(w, request(t.Context(), Zstd), Options{NoCompression: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(Comment("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.Encoding() != "" || w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("encoding %q, header %q", s.Encoding(), w.Header().Get("Content-Encoding"))
	}
	if got := string(w.bytes()); got != ": x\n\n" {
		t.Fatalf("got %q", got)
	}
}

// CloseWith sends its events in the last frame, also after the hub
// stopped, in every coding.
func TestCloseWithDuringShutdown(t *testing.T) {
	for _, enc := range []string{"", Zstd, Brotli, Gzip} {
		ctx, cancel := context.WithCancelCause(t.Context())
		w := newRecorder()
		s, err := Open(w, request(ctx, enc), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Write(Comment("x")); err != nil {
			t.Fatal(err)
		}
		cancel(shutdown.Cause)
		s.abort()
		if err := s.CloseWith(Comment("last")); err != nil {
			t.Fatalf("%q: %v", enc, err)
		}
		var body io.Reader = bytes.NewReader(w.bytes())
		if enc != "" {
			body = decoder(t, enc, w.bytes())
		}
		got, err := io.ReadAll(body)
		if err != nil || string(got) != ": x\n\n: last\n\n" {
			t.Fatalf("%q: got %q, %v", enc, got, err)
		}
		if err := s.Write(Comment("y")); !errors.Is(err, ErrClosed) {
			t.Fatalf("%q: a write after CloseWith: %v", enc, err)
		}
	}
}

// When the hub stops, the end frame waits for a client that takes nothing
// for endTimeout, not for WriteTimeout: it would hold up the shutdown.
func TestShutdownBoundsEndFrame(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	w := newRecorder()
	s, err := Open(w, request(ctx, Zstd), Options{WriteTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(Comment("x")); err != nil {
		t.Fatal(err)
	}
	cancel(shutdown.Cause)
	s.abort()
	w.stall(true)
	start := time.Now()
	err = s.CloseWith(Comment("last"))
	if d := time.Since(start); d < endTimeout || d > endTimeout+time.Second {
		t.Fatalf("the end frame took %v", d)
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) || !strings.Contains(err.Error(), endTimeout.String()) {
		t.Fatalf("CloseWith returned %v", err)
	}
}

// When the hub stops, a write under way fails at once, as when the client
// leaves, and Close sends nothing after it.
func TestShutdownCutsWriteUnderWay(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	w := newRecorder()
	s, err := Open(w, request(ctx, Zstd), Options{WriteTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	w.stall(true)
	done := make(chan error, 1)
	go func() { done <- s.Write(Comment("x")) }()
	<-w.blocked
	cancel(shutdown.Cause)
	select {
	case err := <-done:
		if !errors.Is(err, shutdown.Cause) {
			t.Fatalf("the write failed with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the write outlived the shutdown")
	}
	sent := len(w.bytes())
	w.stall(false)
	s.Close()
	if n := len(w.bytes()) - sent; n > 0 {
		t.Fatalf("Close wrote %d bytes after a cut write", n)
	}
}

// A stream whose request is already over fails to open.
func TestOpenAfterContextEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	w := newRecorder()
	w.stall(true)
	if s, err := Open(w, request(ctx, "br"), Options{}); err == nil {
		s.Close()
		t.Fatal("opened")
	}
}

func TestUnsupportedDeadlineIsIgnored(t *testing.T) {
	w := httptest.NewRecorder()
	s, err := Open(w, request(t.Context(), "gzip"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(Comment("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(decoder(t, Gzip, w.Body.Bytes()))
	if err != nil || string(got) != ": x\n\n" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// lateDeadlines counts SetWriteDeadline calls after its owner marked the
// response as finished.
type lateDeadlines struct {
	*recorder
	finished bool
	late     int
}

func (l *lateDeadlines) SetWriteDeadline(t time.Time) error {
	if l.finished {
		l.late++
	}
	return l.recorder.SetWriteDeadline(t)
}

// The end of the request's context can reach the stream after Close has
// returned, when the handler may have returned too and its connection may
// serve another request: the stream must leave the response alone.
func TestAbortAfterCloseLeavesResponseAlone(t *testing.T) {
	w := &lateDeadlines{recorder: newRecorder()}
	s, err := Open(w, request(t.Context(), "zstd"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	w.finished = true
	s.abort()
	if w.late > 0 {
		t.Fatal("set a write deadline after Close")
	}
}
