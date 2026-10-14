// Package sse writes Datastar server-sent events through a compressor of
// its own per stream.
//
// Events are formatted once (PatchElements, PatchSignals, Comment) and the
// same bytes can go to any number of streams. Each stream compresses them
// with a window that spans the whole stream, so a frame that repeats much
// of an earlier one costs little on the wire. The encoders are tuned for
// many long-lived streams: zstd at its fastest level (Options.ZstdLevel)
// with a 512 KiB window (about 1.7 MiB of heap per stream, 1 MiB with
// Options.ZstdLowMemory) when the client accepts it, else brotli at
// quality 4 with a 256 KiB window (about 2 MiB), else gzip.
//
// A Budget shares a memory limit among many streams, their encoders and
// the frames kept for them: each stream gets a window that holds its
// frames, zstd streams move to smaller windows as more streams open, and
// brotli streams take quality 3, then a window below their frames, as the
// limit fills. Stream.Hibernate lets an idle zstd stream give its encoder
// back until its next write.
//
// Every write has its own deadline, so run the server without a
// WriteTimeout and let a stream that cannot take a write in time fail
// alone. Pages open their streams with Datastar's retry: 'error' from an
// element with an id: a server that ends its streams to restart sends
// Reconnect last (live does, with Spec.Reconnect), and 'error' carries the
// page's request through the restart. The default retry reopens neither.
package sse

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"

	"github.com/zweiundeins/lith/internal/shutdown"
)

// Options tune a stream. The zero value is ready to use.
type Options struct {
	// WriteTimeout bounds each write to the client (default 10s). A
	// client that does not take a write in time loses its stream. The end
	// frame of a stream the hub stops waits 250 ms at most.
	WriteTimeout time.Duration
	// Compress, if set, is a semaphore shared by streams: its capacity
	// bounds how many compress at once. A slot is held only while
	// compressing, never during the network write.
	Compress chan struct{}
	// ZstdWindow is the base 2 logarithm of zstd's window (default 19).
	// With a Budget that has a limit, it is the largest window the budget
	// gives, and unset, the budget's MaxWindow.
	ZstdWindow int
	// ZstdLevel is zstd's level (default zstd.SpeedFastest). A higher one
	// sends fewer bytes per frame for more CPU and heap per stream:
	// bench/RESULTS.md has the trade-off measured on a shared board.
	ZstdLevel zstd.EncoderLevel
	// ZstdLowMemory keeps zstd's history at its window: about 1 MiB per
	// stream instead of 1.65 MiB, for twice the CPU per frame once the
	// streams no longer fit in the CPU's cache (measured with 200 streams),
	// since the encoder then shifts its window after every 128 KiB of input.
	ZstdLowMemory bool
	// BrotliQuality is brotli's quality, 1 to 11 (default 4).
	BrotliQuality int
	// BrotliWindow is the base 2 logarithm of brotli's window (default 18).
	// With a Budget that has a limit, it is the largest window the budget
	// gives, and unset, the budget's MaxWindow.
	BrotliWindow int
	// NoCompression sends the stream as it is, whatever the client accepts:
	// for small, frequent events, where an encoder's heap (0.4 MiB and up
	// per stream) costs more than it saves.
	NoCompression bool
	// Budget, if set, is shared by streams: it picks each stream's window
	// within a memory limit, from the size of its writes. ZstdLowMemory
	// keeps a stream to low-memory windows, and BrotliQuality is the
	// quality brotli streams take while the budget has room (else 3).
	Budget *Budget
}

// The content codings a stream can use, in the server's order of
// preference.
const (
	Zstd   = "zstd"
	Brotli = "br"
	Gzip   = "gzip"
)

// ErrClosed is returned by writes to a closed stream.
var ErrClosed = errors.New("sse: stream closed")

type encoder interface {
	Write(p []byte) (int, error)
	Flush() error
	Close() error
}

// Stream is one open event stream. Its methods are not safe for
// concurrent use, except that the stream may end (its request context
// ends) at any time: a write in progress then fails at once.
type Stream struct {
	w        http.ResponseWriter
	rc       *http.ResponseController
	ctx      context.Context
	stop     func() bool
	opt      Options
	encoding string
	enc      encoder
	buf      bytes.Buffer // compressed output, before it goes out
	in, out  int64
	err      error
	zstd     zstdKey // the settings of a zstd stream's encoder
	asleep   bool    // a zstd stream without an encoder: between two frames
	framed   bool    // the zstd encoder has begun a frame
	tier     tier    // what the stream holds of its budget
	user     bool    // the stream counts in its budget's fair share
	seen     int64   // the budget's count of streams without room, at the stream's last tier
	freed    int64   // the budget's count of times room freed up, at the stream's last look
	chunk    int     // the most a brotli encoder takes between two flushes
	frame    int     // the largest write, in bytes of events
	pick     bool    // a brotli stream whose first write picks its tier
	low      bool    // a zstd stream whose window is below its frame
	accepts  string  // the coding the client takes, whatever the budget gave

	mu      sync.Mutex
	writing bool // a write is under way
	closing bool // Close is writing the end frame
	closed  bool // after Close, abort leaves the response alone
}

// Open starts an event stream on w: it picks the encoding from the
// request's Accept-Encoding, sends the headers and flushes them. The
// stream ends when the request's context ends. The handler must Close
// the stream before it returns.
func Open(w http.ResponseWriter, r *http.Request, o Options) (*Stream, error) {
	if err := r.Context().Err(); err != nil {
		return nil, context.Cause(r.Context())
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 10 * time.Second
	}
	if o.ZstdLevel == 0 {
		o.ZstdLevel = zstd.SpeedFastest
	}
	if o.BrotliQuality == 0 {
		o.BrotliQuality = 4
	}
	if o.Budget == nil {
		// A budget with a limit takes unset windows as any up to its own
		// largest, and one without as these.
		o.ZstdWindow = cmp.Or(o.ZstdWindow, 19)
		o.BrotliWindow = cmp.Or(o.BrotliWindow, 18)
	}
	s := &Stream{
		w:       w,
		rc:      http.NewResponseController(w),
		ctx:     r.Context(),
		opt:     o,
		accepts: Negotiate(r.Header.Get("Accept-Encoding")),
	}
	if !o.NoCompression {
		s.encoding = s.accepts
	}
	if b := o.Budget; b != nil {
		t, user, ok := b.open(s.encoding, o, &s.seen)
		if !ok {
			return nil, ErrNoRoom
		}
		s.tier, s.user = t, user
		s.pick = t == brotliFloor
		if t.coding == "" {
			s.asleep = s.encoding == Zstd
			if !s.asleep {
				s.encoding = ""
			}
		}
	}
	var err error
	switch s.encoding {
	case Zstd:
		s.zstd = zstdKey{o.ZstdLevel, o.ZstdWindow, o.ZstdLowMemory}
		if s.tier.coding == Zstd {
			s.zstd.window, s.zstd.lowMem = s.tier.window, s.tier.lowMem
		}
		if !s.asleep {
			s.enc, err = s.zstd.encoder(&s.buf)
		} else if o.ZstdLevel < zstd.SpeedFastest || o.ZstdLevel > zstd.SpeedBestCompression {
			err = fmt.Errorf("sse: unknown zstd level %d", o.ZstdLevel)
		}
	case Brotli:
		if !s.pick {
			s.startBrotli()
		}
	case Gzip:
		s.enc, err = gzip.NewWriterLevel(&s.buf, gzip.BestSpeed)
	}
	if err != nil {
		if o.Budget != nil {
			o.Budget.close(s.tier, s.encoding, s.user)
		}
		return nil, err
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	h.Add("Vary", "Accept-Encoding")
	if s.encoding != "" {
		h.Set("Content-Encoding", s.encoding)
	}
	s.stop = context.AfterFunc(s.ctx, s.abort)
	if err := s.send(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Encoding is the stream's content coding: Zstd, Brotli, Gzip or "".
func (s *Stream) Encoding() string { return s.encoding }

// Sent reports the bytes of events written to the stream and the bytes
// that went out for them, compressed.
func (s *Stream) Sent() (events, wire int64) { return s.in, s.out }

// Tier names what the stream's encoder holds now, as BudgetStats.Streams
// counts it: "zstd 2^19", "zstd 2^18 low memory", "br q4 2^18" or "gzip";
// "zstd asleep" while a zstd stream has no encoder (before its first
// write, while it hibernates, or while its budget has no room for it);
// "identity" when it goes out uncompressed. heap is what the stream's
// budget reserves for the encoder: 0 without a budget, and for a stream
// asleep or uncompressed. A brotli stream with a budget holds quality 3 at
// 2^16 until its first write picks its tier.
func (s *Stream) Tier() (name string, heap int64) {
	if s.opt.Budget != nil {
		return label(s.tier, s.encoding), s.tier.heap
	}
	t := tier{coding: s.encoding}
	switch s.encoding {
	case Zstd:
		t.window, t.lowMem = s.zstd.window, s.zstd.lowMem
		if s.asleep {
			t = tier{}
		}
	case Brotli:
		t.window, t.quality = s.opt.BrotliWindow, s.opt.BrotliQuality
	}
	return label(t, s.encoding), 0
}

// Budget is the budget the stream shares with other streams
// (Options.Budget), nil for none.
func (s *Stream) Budget() *Budget { return s.opt.Budget }

// Write sends events: it compresses them together, flushes the encoder
// and writes the result to the client within the write timeout. A write
// the client does not take in time fails with an error that wraps
// os.ErrDeadlineExceeded, and one cut short by the end of the request's
// context with the context's cause. After a failed write the stream is
// broken, and every later write returns the same error.
func (s *Stream) Write(events ...[]byte) error {
	if s.err != nil {
		return s.err
	}
	n := 0
	for _, e := range events {
		n += len(e)
	}
	if n == 0 {
		return nil
	}
	if s.ctx.Err() != nil {
		return s.ended()
	}
	if n > s.frame {
		s.grew(n)
	}
	if s.encoding == "" {
		s.in += int64(n)
		return s.send(events...)
	}
	if s.pick {
		s.tier = s.opt.Budget.first(s.tier, s.opt, s.frame)
		s.startBrotli()
	}
	if s.asleep {
		ok, err := s.wake()
		if err != nil {
			return s.fail(err)
		}
		if !ok {
			s.in += int64(n)
			return s.send(rawFrame(events, n)...)
		}
	}
	if s.opt.Compress != nil {
		select {
		case s.opt.Compress <- struct{}{}:
		case <-s.ctx.Done():
			return s.ended()
		}
	}
	s.in += int64(n)
	s.buf.Reset()
	err := s.shrink()
	if err == nil && s.asleep {
		// The budget took the stream's tier: the end of its frame, then the
		// events in a raw one.
		if s.opt.Compress != nil {
			<-s.opt.Compress
		}
		return s.send(append([][]byte{s.buf.Bytes()}, rawFrame(events, n)...)...)
	}
	s.framed = true
	if err == nil {
		err = s.compress(events)
	}
	if err == nil {
		err = s.enc.Flush()
	}
	if s.opt.Compress != nil {
		<-s.opt.Compress
	}
	if err != nil {
		return s.fail(err)
	}
	return s.send(s.buf.Bytes())
}

// grew notes a write of n bytes, larger than any before.
func (s *Stream) grew(n int) {
	s.frame = n
	if s.tier.coding == Zstd && !s.low && s.opt.Budget.short(s.tier, Zstd, s.opt, n) {
		s.low, s.freed = true, -1
	}
}

// startBrotli makes a brotli stream's encoder, of its tier or, without a
// budget, of its options.
func (s *Stream) startBrotli() {
	q, lgwin := s.opt.BrotliQuality, s.opt.BrotliWindow
	if s.tier.coding == Brotli {
		q, lgwin = s.tier.quality, s.tier.window
	}
	s.enc = brotli.NewWriterOptions(&s.buf, brotli.WriterOptions{Quality: q, LGWin: lgwin})
	s.chunk = brotliChunk(q)
	s.pick = false
}

// emptyBrotli is a brotli stream with a window of 2^16 that holds nothing.
var emptyBrotli = []byte{0x06}

// brotliChunk is the most a brotli encoder of quality q takes between two
// flushes: one input block below quality 4, else 32 KiB, which bounds its heap.
func brotliChunk(q int) int {
	if q < 4 {
		return 16 << 10
	}
	return 32 << 10
}

// compress writes events to the encoder, flushing a brotli encoder every
// s.chunk bytes.
func (s *Stream) compress(events [][]byte) error {
	if s.encoding != Brotli {
		for _, e := range events {
			if _, err := s.enc.Write(e); err != nil {
				return err
			}
		}
		return nil
	}
	taken := 0
	for _, e := range events {
		for len(e) > 0 {
			if taken == s.chunk {
				// Unflushed, brotli extends a copy into the next block without
				// hashing it, and after a change loses the frame before.
				if err := s.enc.Flush(); err != nil {
					return err
				}
				taken = 0
			}
			n := min(len(e), s.chunk-taken)
			if _, err := s.enc.Write(e[:n]); err != nil {
				return err
			}
			e, taken = e[n:], taken+n
		}
	}
	return nil
}

// Close ends the compressed stream cleanly when it is still healthy and
// releases the encoder. It does not end the HTTP response: the handler
// does, by returning.
func (s *Stream) Close() error { return s.CloseWith() }

// CloseWith is Close that sends events in the stream's last frame. It
// sends them also when the stream shuts down (the hub stops), as live
// does with Reconnect.
func (s *Stream) CloseWith(events ...[]byte) error {
	if s.stop == nil {
		return nil
	}
	var err error
	clean := false
	if s.err == nil && (s.ctx.Err() == nil || s.shuttingDown()) {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		n := 0
		for _, e := range events {
			n += len(e)
		}
		if s.pick && n > 0 {
			s.frame = max(s.frame, n)
			s.tier = s.opt.Budget.first(s.tier, s.opt, s.frame)
			s.startBrotli()
		}
		if s.pick {
			err = s.send(emptyBrotli)
		} else if s.enc != nil {
			s.buf.Reset()
			if err = s.compress(events); err == nil {
				s.in += int64(n)
			}
			if err == nil {
				err = s.enc.Close()
				clean = err == nil
			}
			if err == nil && s.buf.Len() > 0 {
				err = s.send(s.buf.Bytes())
			}
		} else if n > 0 {
			s.in += int64(n)
			if s.asleep {
				err = s.send(rawFrame(events, n)...)
			} else {
				err = s.send(events...)
			}
		}
	}
	if e, ok := s.enc.(*zstd.Encoder); ok && clean {
		s.zstd.put(e)
	}
	if s.opt.Budget != nil {
		s.opt.Budget.close(s.tier, s.encoding, s.user)
	}
	s.stop()
	s.stop = nil
	s.enc = nil
	s.buf = bytes.Buffer{}
	if s.err == nil {
		s.err = ErrClosed
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return err
}

// endTimeout bounds the end frame of a stream the hub stops. A client that
// still reads takes it at once; one that has stopped would hold up the
// shutdown, and loses its stream instead, which retry 'error' reopens.
const endTimeout = 250 * time.Millisecond

// send writes chunks to the client and flushes them under a write
// deadline, which it clears afterwards: HTTP/2 resets a stream when its
// deadline passes, even while nothing is being written.
func (s *Stream) send(chunks ...[]byte) error {
	timeout := s.opt.WriteTimeout
	if s.closing && s.shuttingDown() {
		timeout = min(timeout, endTimeout)
	}
	due := time.Now().Add(timeout)
	if err := s.start(due); err != nil {
		return s.fail(err)
	}
	for _, c := range chunks {
		n, err := s.w.Write(c)
		s.out += int64(n)
		if err != nil {
			return s.fail(s.finish(s.why(err, due, timeout)))
		}
	}
	if err := s.rc.Flush(); err != nil {
		return s.fail(s.finish(s.why(err, due, timeout)))
	}
	if err := s.finish(nil); err != nil {
		return s.fail(err)
	}
	return nil
}

// start sets the deadline of a write that begins. Once the request's
// context has ended it refuses, but for the end frame of a stream that
// shuts down.
func (s *Stream) start(due time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil && !(s.closing && s.shuttingDown()) {
		return context.Cause(s.ctx)
	}
	if err := s.deadline(due); err != nil {
		return err
	}
	s.writing = true
	return nil
}

// finish ends a write that failed with err, or else clears its deadline.
// A write that the end of the request's context overtook fails even so,
// unless the stream shuts down: its client is still there.
func (s *Stream) finish(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writing = false
	if err != nil {
		return err
	}
	if s.ctx.Err() != nil && !s.shuttingDown() {
		return context.Cause(s.ctx)
	}
	return s.deadline(time.Time{})
}

// why names the cause of a failed write: the client took no bytes until
// the deadline, or the stream ended (HTTP/2 shows both as a closed
// stream), or else err itself.
func (s *Stream) why(err error, due time.Time, timeout time.Duration) error {
	if !time.Now().Before(due) {
		return fmt.Errorf("sse: the client took no bytes for %v: %w", timeout, os.ErrDeadlineExceeded)
	}
	// net/http ends the context before a write to a client that left
	// fails, while abort may not have run yet.
	if s.ctx.Err() != nil {
		return context.Cause(s.ctx)
	}
	return err
}

// deadline sets the write deadline (zero: none). s.mu is held.
func (s *Stream) deadline(t time.Time) error {
	if err := s.rc.SetWriteDeadline(t); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

// abort runs when the request's context ends: a write under way fails at
// once, except the end frame of a stream that shuts down. It leaves an
// idle response alone, so that the response can still end cleanly. Once
// Close has returned, the handler may have returned too, and the response
// is no longer ours.
func (s *Stream) abort() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.writing || (s.closing && s.shuttingDown()) {
		return
	}
	_ = s.rc.SetWriteDeadline(time.Now())
}

// shuttingDown reports whether the request's context ended because the
// hub stops (shutdown.Cause), while the client is still there.
func (s *Stream) shuttingDown() bool {
	return errors.Is(context.Cause(s.ctx), shutdown.Cause)
}

// ended is the error of a write that the end of the request's context
// refused before it began. It breaks the stream, unless the stream shuts
// down: Close still ends that one cleanly.
func (s *Stream) ended() error {
	if s.shuttingDown() {
		return context.Cause(s.ctx)
	}
	return s.fail(context.Cause(s.ctx))
}

func (s *Stream) fail(err error) error {
	if s.err == nil {
		s.err = err
	}
	return s.err
}

// Negotiate picks the coding for an Accept-Encoding header: the
// acceptable one with the highest q-value, zstd before brotli before
// gzip when they tie, or "" for none.
func Negotiate(acceptEncoding string) string {
	best, bestQ := "", 0.0
	for _, c := range [...]string{Zstd, Brotli, Gzip} {
		if q := qvalue(acceptEncoding, c); q > bestQ {
			best, bestQ = c, q
		}
	}
	return best
}

// qvalue is the weight the header gives to coding, from its own entry or
// else from "*" (0 when neither is listed).
func qvalue(header, coding string) float64 {
	star := 0.0
	for part := range strings.SplitSeq(header, ",") {
		name, params, _ := strings.Cut(part, ";")
		name = strings.TrimSpace(name)
		q := 1.0
		for p := range strings.SplitSeq(params, ";") {
			k, v, _ := strings.Cut(p, "=")
			if strings.EqualFold(strings.TrimSpace(k), "q") {
				f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
				if err != nil {
					f = 0
				}
				q = f
			}
		}
		if strings.EqualFold(name, coding) {
			return q
		}
		if name == "*" {
			star = q
		}
	}
	return star
}
