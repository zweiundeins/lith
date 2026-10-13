package sse

import (
	"bytes"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Hibernate ends the stream's zstd frame and gives its encoder to a pool
// shared by streams, and its tier back to its budget, so that an idle
// stream holds neither. The next write takes an encoder from the pool and
// starts a new frame, without the history of the frames before; with a
// budget, of the tier the budget then allows, or in raw blocks while it
// allows none. A Content-Encoding: zstd body may hold any number of frames
// one after another, and browsers decode them as one stream. Hibernate
// does nothing to a stream of another coding: a brotli or gzip body is a
// single stream, which cannot start over without its history.
func (s *Stream) Hibernate() error {
	if s.err != nil {
		return s.err
	}
	if s.encoding != Zstd || s.asleep {
		return nil
	}
	if s.ctx.Err() != nil {
		return s.ended()
	}
	e := s.enc.(*zstd.Encoder)
	s.buf.Reset()
	var err error
	if s.framed {
		err = e.Close()
	}
	if err == nil {
		s.zstd.put(e)
	}
	s.enc, s.asleep, s.framed = nil, true, false
	if b := s.opt.Budget; b != nil {
		b.sleep(s.tier)
		s.tier = tier{}
	}
	if err != nil {
		return s.fail(err)
	}
	if s.buf.Len() > 0 {
		err = s.send(s.buf.Bytes())
	}
	s.buf = bytes.Buffer{}
	return err
}

// wake gives a zstd stream without an encoder one from the pool, of the
// tier its budget allows. It reports false when the budget has no room.
func (s *Stream) wake() (bool, error) {
	if b := s.opt.Budget; b != nil {
		t, ok := b.wake(s.opt, &s.seen)
		if !ok {
			return false, nil
		}
		s.tier = t
		s.zstd.window, s.zstd.lowMem = t.window, t.lowMem
	}
	e, err := s.zstd.encoder(&s.buf)
	if err != nil {
		return false, err
	}
	s.enc, s.asleep = e, false
	return true, nil
}

// shrink moves a zstd stream to a smaller tier when its budget asks: it
// ends the frame in s.buf and begins the next one there, with an encoder of
// the new tier, or with none when the budget, its limit lowered, has no
// room left: the stream then falls asleep. Write calls it before it
// compresses.
func (s *Stream) shrink() error {
	b := s.opt.Budget
	if b == nil || s.tier.coding != Zstd || b.starved.Load() == s.seen {
		return nil
	}
	t, ok := b.shrink(s.tier, s.opt, &s.seen)
	if !ok {
		return nil
	}
	s.tier = t
	old := s.enc.(*zstd.Encoder)
	if s.framed {
		if err := old.Close(); err != nil {
			return err
		}
	}
	s.zstd.put(old)
	s.enc, s.framed = nil, false
	if t.coding == "" {
		s.asleep = true
		return nil
	}
	s.zstd.window, s.zstd.lowMem = t.window, t.lowMem
	e, err := s.zstd.encoder(&s.buf)
	if err != nil {
		return err
	}
	s.enc = e
	return nil
}

// zstdKey holds the settings of a zstd encoder. Encoders with the same
// settings share a pool.
type zstdKey struct {
	level  zstd.EncoderLevel
	window int // base 2 logarithm
	lowMem bool
}

// zstdPools hold the encoders of streams that hibernated or closed, by
// their settings, until a stream takes one or the garbage collector frees
// it.
var zstdPools sync.Map // zstdKey → *sync.Pool

// encoder returns an encoder that writes a new frame to w.
func (k zstdKey) encoder(w io.Writer) (*zstd.Encoder, error) {
	if p, ok := zstdPools.Load(k); ok {
		if e, ok := p.(*sync.Pool).Get().(*zstd.Encoder); ok {
			e.Reset(w)
			return e, nil
		}
	}
	return zstd.NewWriter(w,
		zstd.WithEncoderLevel(k.level),
		zstd.WithWindowSize(1<<k.window),
		zstd.WithEncoderConcurrency(1),
		zstd.WithLowerEncoderMem(k.lowMem),
		zstd.WithEncoderCRC(false))
}

// put gives a closed encoder to the pool.
func (k zstdKey) put(e *zstd.Encoder) {
	e.Reset(nil)
	p, _ := zstdPools.LoadOrStore(k, new(sync.Pool))
	p.(*sync.Pool).Put(e)
}

// rawFrame is a zstd frame that holds n bytes of events as they are, in
// raw blocks (RFC 8878, 3.1.1): what a zstd stream without an encoder
// sends. It returns the frame in pieces, which refer to the events.
func rawFrame(events [][]byte, n int) [][]byte {
	const maxBlock = 128 << 10
	// A single segment: the content size is the window, and no block may
	// exceed it.
	hdr := []byte{0x28, 0xb5, 0x2f, 0xfd}
	switch {
	case n < 256:
		hdr = append(hdr, 0x20, byte(n))
	case n < 1<<16+256:
		m := n - 256
		hdr = append(hdr, 0x60, byte(m), byte(m>>8))
	default:
		hdr = append(hdr, 0xa0, byte(n), byte(n>>8), byte(n>>16), byte(n>>24))
	}
	out := make([][]byte, 0, len(events)+2)
	i, j := 0, 0 // the event and the offset in it where the next block starts
	for left := n; left > 0; {
		size := min(left, maxBlock)
		left -= size
		h := uint32(size) << 3 // Raw_Block
		if left == 0 {
			h |= 1 // Last_Block
		}
		out = append(out, append(hdr, byte(h), byte(h>>8), byte(h>>16)))
		hdr = nil
		for size > 0 {
			k := min(size, len(events[i])-j)
			if k > 0 {
				out = append(out, events[i][j:j+k])
			}
			size -= k
			if j += k; j == len(events[i]) {
				i, j = i+1, 0
			}
		}
	}
	return out
}
