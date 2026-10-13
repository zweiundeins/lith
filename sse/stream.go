// Package sse writes Datastar server-sent events through a compressor of
// its own per stream.
//
// Events are formatted once (PatchElements, PatchSignals, Comment) and the
// same bytes can go to any number of streams. Each stream compresses them
// with a window that spans the whole stream, so a frame that repeats much
// of an earlier one costs little on the wire. The encoders are tuned for
// many long-lived streams: zstd at its fastest level with a 512 KiB window
// (about 1 MiB of heap per stream) when the client accepts it, else brotli
// at quality 4 with a 256 KiB window (about 2 MiB), else gzip.
//
// Every write has its own deadline, so run the server without a
// WriteTimeout and let a stream that cannot take a write in time fail
// alone. Pages should open their streams with Datastar's retry: 'always',
// since the default does not reopen a stream that the server ends.
package sse

import (
	"bytes"
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
	// client that does not take a write in time loses its stream.
	WriteTimeout time.Duration
	// Compress, if set, is a semaphore shared by streams: its capacity
	// bounds how many compress at once. A slot is held only while
	// compressing, never during the network write.
	Compress chan struct{}
	// ZstdWindow is the base 2 logarithm of zstd's window (default 19).
	ZstdWindow int
	// ZstdLowMemory keeps zstd's history at its window: about 1 MiB per
	// stream instead of 1.65 MiB, for twice the CPU per frame once the
	// streams no longer fit in the CPU's cache (measured with 200 streams),
	// since the encoder then shifts its window after every 128 KiB of input.
	ZstdLowMemory bool
	// BrotliQuality is brotli's quality, 1 to 11 (default 4).
	BrotliQuality int
	// BrotliWindow is the base 2 logarithm of brotli's window (default 18).
	BrotliWindow int
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
	if o.ZstdWindow == 0 {
		o.ZstdWindow = 19
	}
	if o.BrotliQuality == 0 {
		o.BrotliQuality = 4
	}
	if o.BrotliWindow == 0 {
		o.BrotliWindow = 18
	}
	s := &Stream{
		w:        w,
		rc:       http.NewResponseController(w),
		ctx:      r.Context(),
		opt:      o,
		encoding: Negotiate(r.Header.Get("Accept-Encoding")),
	}
	var err error
	switch s.encoding {
	case Zstd:
		s.enc, err = zstd.NewWriter(&s.buf,
			zstd.WithEncoderLevel(zstd.SpeedFastest),
			zstd.WithWindowSize(1<<o.ZstdWindow),
			zstd.WithEncoderConcurrency(1),
			zstd.WithLowerEncoderMem(o.ZstdLowMemory),
			zstd.WithEncoderCRC(false))
	case Brotli:
		s.enc = brotli.NewWriterOptions(&s.buf, brotli.WriterOptions{Quality: o.BrotliQuality, LGWin: o.BrotliWindow})
	case Gzip:
		s.enc, err = gzip.NewWriterLevel(&s.buf, gzip.BestSpeed)
	}
	if err != nil {
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
	if s.enc == nil {
		s.in += int64(n)
		return s.send(events...)
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
	var err error
	for _, e := range events {
		if _, err = s.enc.Write(e); err != nil {
			break
		}
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

// Close ends the compressed stream cleanly when it is still healthy and
// releases the encoder. It does not end the HTTP response: the handler
// does, by returning.
func (s *Stream) Close() error {
	if s.stop == nil {
		return nil
	}
	var err error
	if s.enc != nil && s.err == nil && (s.ctx.Err() == nil || s.shuttingDown()) {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		s.buf.Reset()
		if err = s.enc.Close(); err == nil && s.buf.Len() > 0 {
			err = s.send(s.buf.Bytes())
		}
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

// send writes chunks to the client and flushes them under a write
// deadline, which it clears afterwards: HTTP/2 resets a stream when its
// deadline passes, even while nothing is being written.
func (s *Stream) send(chunks ...[]byte) error {
	due := time.Now().Add(s.opt.WriteTimeout)
	if err := s.start(due); err != nil {
		return s.fail(err)
	}
	for _, c := range chunks {
		n, err := s.w.Write(c)
		s.out += int64(n)
		if err != nil {
			return s.fail(s.finish(s.why(err, due)))
		}
	}
	if err := s.rc.Flush(); err != nil {
		return s.fail(s.finish(s.why(err, due)))
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
func (s *Stream) why(err error, due time.Time) error {
	if !time.Now().Before(due) {
		return fmt.Errorf("sse: the client took no bytes for %v: %w", s.opt.WriteTimeout, os.ErrDeadlineExceeded)
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
