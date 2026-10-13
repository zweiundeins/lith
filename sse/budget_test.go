package sse

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// openWith opens a stream on a new recorder for a client that accepts
// the coding.
func openWith(t *testing.T, coding string, o Options) (*Stream, *recorder) {
	t.Helper()
	w := newRecorder()
	s, err := Open(w, request(t.Context(), coding), o)
	if err != nil {
		t.Fatal(err)
	}
	return s, w
}

// held lists what the open streams of the budget hold, as Stats does.
func held(b *Budget) map[string]int { return b.Stats().Streams }

// Streams that open and close in any order never reserve more than the
// limit, and give all of it back.
func TestBudgetNeverExceedsLimit(t *testing.T) {
	const limit = 24 << 20
	b := NewBudget(BudgetOptions{Limit: limit, MinWindow: 15, MaxWindow: 20})
	rnd := rand.New(rand.NewPCG(1, 2))
	codings := []string{Zstd, Brotli, "br;q=1, gzip;q=0.5", Gzip, ""}
	var open []*Stream
	kinds := map[string]bool{}
	for i := range 600 {
		switch r := rnd.IntN(8); {
		case len(open) > 0 && r < 1:
			k := rnd.IntN(len(open))
			open[k].Close()
			open = append(open[:k], open[k+1:]...)
		case len(open) > 0 && r < 4: // a write, which may wake or shrink a zstd stream
			if err := open[rnd.IntN(len(open))].Write(frame(i)); err != nil {
				t.Fatal(err)
			}
		case len(open) > 0 && r < 5:
			if err := open[rnd.IntN(len(open))].Hibernate(); err != nil {
				t.Fatal(err)
			}
		default:
			s, _ := openWith(t, codings[rnd.IntN(len(codings))], Options{Budget: b, BrotliQuality: 3 + rnd.IntN(2)})
			open = append(open, s)
		}
		st := b.Stats()
		if st.Reserved > limit {
			t.Fatalf("step %d: %d bytes reserved, over the limit of %d", i, st.Reserved, limit)
		}
		n, sum := 0, int64(0)
		for k, c := range st.Streams {
			n += c
			kinds[k] = true
		}
		for _, s := range open {
			sum += s.tier.heap
		}
		if n != len(open) || sum != st.Reserved {
			t.Fatalf("step %d: the budget counts %d streams and %d bytes, the %d open streams hold %d", i, n, st.Reserved, len(open), sum)
		}
	}
	if len(kinds) < 8 {
		t.Fatalf("the streams held too few kinds of tier: %v", kinds)
	}
	for _, s := range open {
		s.Close()
	}
	if st := b.Stats(); st.Reserved != 0 || len(st.Streams) != 0 {
		t.Fatalf("after every stream closed: %+v", st)
	}
}

// A stream gets the largest tier within its fair share and what is left,
// between MinWindow and MaxWindow. The fair share shrinks as streams open;
// once it falls below MinWindow's tier, a stream gets that tier for as long
// as what is left holds it, and none after that.
func TestBudgetTiers(t *testing.T) {
	b := NewBudget(BudgetOptions{Limit: 8 << 20, MinWindow: 18, MaxWindow: 19})
	var open []*Stream
	var got []string
	for range 13 {
		s, _ := openWith(t, Zstd, Options{Budget: b})
		open = append(open, s)
		got = append(got, label(s.tier, s.encoding))
	}
	defer func() {
		for _, s := range open {
			s.Close()
		}
	}()
	want := strings.Repeat("zstd 2^19, ", 4) + // shares of 8, 4, 2.7 and 2 MiB
		"zstd 2^19 low memory, " + // 1.6 MiB, with 1.4 MiB left
		strings.TrimSuffix(strings.Repeat("zstd asleep, ", 8), ", ") // 304 KiB left
	if strings.Join(got, ", ") != want {
		t.Fatalf("tiers:\n got %s\nwant %s", strings.Join(got, ", "), want)
	}
	if r := b.Stats().Reserved; r != 4*1696*kib+1104*kib {
		t.Fatalf("reserved %d", r)
	}

	// Four streams leave: a new one's fair share, 8 MiB / 10, is below the
	// floor, so it gets the floor, and so do the streams that were waiting
	// as they write, until what is left no longer holds it.
	for _, s := range open[:4] {
		s.Close()
	}
	open = open[4:]
	s, _ := openWith(t, Zstd, Options{Budget: b})
	open = append(open, s)
	if got := label(s.tier, s.encoding); got != "zstd 2^18 low memory" {
		t.Fatalf("below the floor: %s", got)
	}
	for _, s := range open[1:9] {
		if err := s.Write(Comment("x")); err != nil {
			t.Fatal(err)
		}
	}
	if got := held(b); got["zstd 2^18 low memory"] != 8 || got["zstd asleep"] != 1 || got["zstd 2^19 low memory"] != 1 {
		t.Fatalf("after the waiting streams wrote: %v", got)
	}
	if r := b.Stats().Reserved; r != 1104*kib+8*848*kib {
		t.Fatalf("reserved %d", r)
	}

	// The clamps hold for brotli and without a limit too; the stream's own
	// BrotliWindow is the largest it gets.
	for _, c := range []struct {
		o    BudgetOptions
		opt  Options
		want string
	}{
		{BudgetOptions{Limit: -1}, Options{}, "br q4 2^18"},
		{BudgetOptions{Limit: -1}, Options{BrotliWindow: 19}, "br q4 2^19"},
		{BudgetOptions{Limit: -1, MaxWindow: 18}, Options{}, "br q4 2^18"},
		{BudgetOptions{Limit: -1, MaxWindow: 18}, Options{BrotliQuality: 3}, "br q3 2^18"},
		{BudgetOptions{Limit: 1 << 30, MinWindow: 16, MaxWindow: 16}, Options{}, "br q4 2^16"},
		{BudgetOptions{Limit: 1 << 30, MaxWindow: 16}, Options{}, "br q4 2^18"},
	} {
		s, _ := openWith(t, Brotli, Options{Budget: NewBudget(c.o), BrotliQuality: c.opt.BrotliQuality, BrotliWindow: c.opt.BrotliWindow})
		if got := label(s.tier, s.encoding); got != c.want {
			t.Errorf("%+v: got %s, want %s", c.o, got, c.want)
		}
		s.Close()
	}
	// With ZstdLowMemory, zstd gets low-memory tiers only.
	s, _ = openWith(t, Zstd, Options{Budget: NewBudget(BudgetOptions{Limit: -1}), ZstdLowMemory: true})
	if got := label(s.tier, s.encoding); got != "zstd 2^19 low memory" {
		t.Errorf("with ZstdLowMemory: %s", got)
	}
	s.Close()
}

// Streams that took large tiers while few were open move down at their
// next write once a stream finds no room, and that stream gets room at its
// next write. Every body still decodes to what was written.
func TestBudgetShrinks(t *testing.T) {
	b := NewBudget(BudgetOptions{Limit: 4 << 20, MinWindow: 17, MaxWindow: 19})
	type stream struct {
		s    *Stream
		w    *recorder
		want []byte
	}
	var ss []*stream
	write := func(x *stream, i int) {
		t.Helper()
		f := frame(i)
		if err := x.s.Write(f); err != nil {
			t.Fatal(err)
		}
		x.want = append(x.want, f...)
	}
	for range 3 {
		s, w := openWith(t, Zstd, Options{Budget: b})
		x := &stream{s: s, w: w}
		ss = append(ss, x)
		write(x, 0)
		write(x, 1)
	}
	// Shares of 4 and 2 MiB; the third, 1.3 MiB, finds 0.7 MiB left.
	if got := held(b); got["zstd 2^19"] != 2 || got["zstd asleep"] != 1 {
		t.Fatalf("before: %v", got)
	}
	for _, x := range ss {
		write(x, 2)
	}
	if got := held(b); got["zstd 2^19 low memory"] != 3 || len(got) != 1 {
		t.Fatalf("after: %v", got)
	}
	for _, x := range ss {
		write(x, 3)
		x.s.Close()
		got, err := io.ReadAll(decoder(t, Zstd, x.w.bytes()))
		if err != nil || !bytes.Equal(got, x.want) {
			t.Fatalf("a stream decodes to %d bytes of %d: %v", len(got), len(x.want), err)
		}
	}
}

// A lower limit moves zstd streams down at their next write, and those
// it leaves no room for send raw frames; every body still decodes.
func TestBudgetSetLimit(t *testing.T) {
	b := NewBudget(BudgetOptions{Limit: 8 << 20})
	var ss []*Stream
	var ws []*recorder
	var want []byte
	round := func(i int) {
		t.Helper()
		for _, s := range ss {
			if err := s.Write(frame(i)); err != nil {
				t.Fatal(err)
			}
		}
		want = append(want, frame(i)...)
	}
	for range 4 {
		s, w := openWith(t, Zstd, Options{Budget: b})
		ss, ws = append(ss, s), append(ws, w)
	}
	round(0)
	if got := held(b); got["zstd 2^19"] != 4 {
		t.Fatalf("before: %v", got)
	}
	b.SetLimit(4 << 20)
	round(1)
	if got := held(b); got["zstd 2^18 low memory"] != 4 {
		t.Fatalf("at 4 MiB: %v", got)
	}
	b.SetLimit(2 << 20)
	round(2)
	if got, st := held(b), b.Stats(); got["zstd 2^18 low memory"] != 2 || got["zstd asleep"] != 2 || st.Reserved > 2<<20 {
		t.Fatalf("at 2 MiB: %v, %d bytes reserved", got, st.Reserved)
	}
	round(3)
	for i, s := range ss {
		s.Close()
		got, err := io.ReadAll(decoder(t, Zstd, ws[i].bytes()))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("stream %d decodes to %d bytes of %d: %v", i, len(got), len(want), err)
		}
	}
}

// A stream that hibernates takes the tier of the fair share of the moment
// it wakes: here the largest, which it did not get when it opened.
func TestBudgetWakeTakesTheShareOfTheMoment(t *testing.T) {
	b := NewBudget(BudgetOptions{Limit: 3 << 20})
	first, _ := openWith(t, Zstd, Options{Budget: b})
	s, w := openWith(t, Zstd, Options{Budget: b})
	if got := label(s.tier, s.encoding); got != "zstd 2^19 low memory" {
		t.Fatalf("opened with %s", got)
	}
	s.Write(frame(1))
	first.Close()
	s.Hibernate()
	s.Write(frame(2))
	if got := label(s.tier, s.encoding); got != "zstd 2^19" {
		t.Fatalf("woke with %s", got)
	}
	s.Close()
	got, err := io.ReadAll(decoder(t, Zstd, w.bytes()))
	if err != nil || !bytes.Equal(got, append(frame(1), frame(2)...)) {
		t.Fatalf("the stream decodes to %d bytes: %v", len(got), err)
	}
}

// A stream the budget has no room for goes out uncompressed: zstd in raw
// blocks, so that it can compress once room frees up, brotli and gzip as
// they are. With Refuse it is turned away, before anything is written.
func TestBudgetFull(t *testing.T) {
	b := NewBudget(BudgetOptions{Limit: 1 << 20, MinWindow: 15, MaxWindow: 15})
	first, _ := openWith(t, Zstd, Options{Budget: b})
	if got := label(first.tier, first.encoding); got != "zstd 2^15 low memory" {
		t.Fatalf("the first stream got %s", got)
	}

	br, w := openWith(t, Brotli, Options{Budget: b})
	if br.Encoding() != "" || w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("a brotli stream without room: %q", br.Encoding())
	}
	br.Close()

	z, w := openWith(t, Zstd, Options{Budget: b})
	if z.Encoding() != Zstd || !z.asleep {
		t.Fatalf("a zstd stream without room: %q, asleep %v", z.Encoding(), z.asleep)
	}
	var want []byte
	for i := range 3 {
		f := frame(i)
		if err := z.Write(f); err != nil {
			t.Fatal(err)
		}
		want = append(want, f...)
	}
	if _, wire := z.Sent(); wire < int64(len(want)) {
		t.Fatalf("%d bytes on the wire for %d bytes of events: compressed without room", wire, len(want))
	}
	// Room frees up: the next write compresses.
	first.Close()
	if err := z.Write(frame(3)); err != nil {
		t.Fatal(err)
	}
	want = append(want, frame(3)...)
	if z.asleep || label(z.tier, z.encoding) != "zstd 2^15 low memory" {
		t.Fatalf("after room freed up: asleep %v, %s", z.asleep, label(z.tier, z.encoding))
	}
	if err := z.CloseWith(Comment("bye")); err != nil {
		t.Fatal(err)
	}
	want = append(want, Comment("bye")...)
	got, err := io.ReadAll(decoder(t, Zstd, w.bytes()))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the stream decodes to %d bytes of %d: %v", len(got), len(want), err)
	}

	refusing := NewBudget(BudgetOptions{Limit: 1 << 20, MinWindow: 15, MaxWindow: 15, Refuse: true})
	keep, _ := openWith(t, Zstd, Options{Budget: refusing})
	defer keep.Close()
	w = newRecorder()
	if _, err := Open(w, request(t.Context(), Brotli), Options{Budget: refusing}); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("Open without room: %v", err)
	}
	if len(w.Header()) != 0 || len(w.bytes()) != 0 {
		t.Fatalf("a refused stream wrote %v, %q", w.Header(), w.bytes())
	}
	if st := refusing.Stats(); st.Refused != 1 || len(st.Streams) != 1 {
		t.Fatalf("stats after a refusal: %+v", st)
	}
	// A stream that compresses nothing needs no room.
	id, _ := openWith(t, "identity", Options{Budget: refusing})
	id.Close()
}

// failingFlush is a client whose flushes fail.
type failingFlush struct{ *recorder }

func (f failingFlush) FlushError() error { return errors.New("flush: broken pipe") }

// A stream gives its reservation back when it closes, and when Open
// fails after reserving one.
func TestBudgetRelease(t *testing.T) {
	b := NewBudget(BudgetOptions{Limit: 64 << 20})
	if _, err := Open(failingFlush{newRecorder()}, request(t.Context(), Zstd), Options{Budget: b}); err == nil {
		t.Fatal("Open succeeded on a broken client")
	}
	if _, err := Open(newRecorder(), request(t.Context(), Zstd), Options{Budget: b, ZstdLevel: 99}); err == nil {
		t.Fatal("Open succeeded with an unknown zstd level")
	}
	if st := b.Stats(); st.Reserved != 0 || len(st.Streams) != 0 {
		t.Fatalf("after failed opens: %+v", st)
	}
	s, _ := openWith(t, Zstd, Options{Budget: b})
	if b.Stats().Reserved == 0 {
		t.Fatal("an open stream reserved nothing")
	}
	s.Close()
	s.Close()
	if st := b.Stats(); st.Reserved != 0 || len(st.Streams) != 0 {
		t.Fatalf("after Close: %+v", st)
	}
}

// A zstd stream that hibernates ends its frame and holds no encoder; its
// next write starts a new frame, and the body decodes to every event. With
// a budget it gives its tier back, and takes one again when it wakes.
func TestHibernate(t *testing.T) {
	b := NewBudget(BudgetOptions{Limit: 64 << 20})
	s, w := openWith(t, Zstd, Options{Budget: b})
	var want []byte
	write := func(e []byte) {
		t.Helper()
		if err := s.Write(e); err != nil {
			t.Fatal(err)
		}
		want = append(want, e...)
	}
	if err := s.Hibernate(); err != nil || len(w.bytes()) != 0 {
		t.Fatalf("hibernating before any write: %v, %d bytes", err, len(w.bytes()))
	}
	for i := range 3 {
		write(frame(i))
		write(frame(i + 1))
		if got := held(b); got["zstd 2^19"] != 1 {
			t.Fatalf("awake: %v", got)
		}
		sent := len(w.bytes())
		if err := s.Hibernate(); err != nil {
			t.Fatal(err)
		}
		if end := w.bytes()[sent:]; !bytes.Equal(end, []byte{1, 0, 0}) {
			t.Fatalf("hibernating sent %x, want the end of the frame", end)
		}
		if s.enc != nil || s.buf.Cap() != 0 || b.Stats().Reserved != 0 || held(b)["zstd asleep"] != 1 {
			t.Fatalf("asleep: encoder %v, buffer %d, %+v", s.enc != nil, s.buf.Cap(), b.Stats())
		}
		if err := s.Hibernate(); err != nil {
			t.Fatal(err)
		}
	}
	write(Comment("last"))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(w.bytes(), []byte{0x28, 0xb5, 0x2f, 0xfd}); n != 4 {
		t.Fatalf("%d frames, want 4", n)
	}
	got, err := io.ReadAll(decoder(t, Zstd, w.bytes()))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the stream decodes to %d bytes of %d: %v", len(got), len(want), err)
	}
	if st := b.Stats(); st.Reserved != 0 || len(st.Streams) != 0 {
		t.Fatalf("after Close: %+v", st)
	}

	// Other codings keep their encoder.
	for _, coding := range []string{Brotli, Gzip, ""} {
		s, w := openWith(t, coding, Options{})
		s.Write(frame(1))
		if err := s.Hibernate(); err != nil {
			t.Fatal(err)
		}
		s.Write(frame(2))
		s.Close()
		got, err := io.ReadAll(decoder(t, coding, w.bytes()))
		if err != nil || !bytes.Equal(got, append(frame(1), frame(2)...)) {
			t.Fatalf("%q: %d bytes, %v", coding, len(got), err)
		}
	}
}

// Raw frames of any size decode to what they hold.
func TestRawFrame(t *testing.T) {
	for _, n := range []int{1, 255, 256, 1000, 65791, 65792, 128 << 10, 128<<10 + 1, 300 << 10} {
		b := bytes.Repeat([]byte("0123456789abcdef"), n/16+1)[:n]
		events := [][]byte{b[:n/3], {}, b[n/3 : n/2], b[n/2:]}
		var frame []byte
		for _, p := range rawFrame(events, n) {
			frame = append(frame, p...)
		}
		got, err := io.ReadAll(decoder(t, Zstd, frame))
		if err != nil || !bytes.Equal(got, b) {
			t.Fatalf("%d bytes: got %d, %v", n, len(got), err)
		}
	}
}

func TestMemoryLimit(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "cgroup")
	os.WriteFile(self, []byte("0::/app.service\n"), 0o644)
	group := filepath.Join(dir, "app.service")
	os.Mkdir(group, 0o755)
	for _, c := range []struct {
		max, high string
		want      int64
	}{
		{"max\n", "max\n", 0},
		{"1073741824\n", "max\n", 1 << 30},
		{"max\n", "536870912\n", 512 << 20},
		{"1073741824\n", "536870912\n", 512 << 20},
	} {
		os.WriteFile(filepath.Join(group, "memory.max"), []byte(c.max), 0o644)
		os.WriteFile(filepath.Join(group, "memory.high"), []byte(c.high), 0o644)
		if got := cgroupLimit(self, dir); got != c.want {
			t.Errorf("max %q, high %q: %d, want %d", c.max, c.high, got, c.want)
		}
	}
	if got := cgroupLimit(filepath.Join(dir, "none"), dir); got != 0 {
		t.Errorf("without a cgroup: %d", got)
	}

	old := debug.SetMemoryLimit(1 << 30)
	defer debug.SetMemoryLimit(old)
	if got := memoryLimit(); got != 1<<30 {
		t.Fatalf("with GOMEMLIMIT: %d", got)
	}
	if got := NewBudget(BudgetOptions{}).Stats().Limit; got != 512<<20 {
		t.Fatalf("the default budget: %d", got)
	}
}

// A budget without a limit gives each stream what its own options ask
// for, so the budget a live hub makes by default changes nothing where the
// process has no memory limit.
func TestBudgetWithoutLimitKeepsTheStreamsOptions(t *testing.T) {
	b := NewBudget(BudgetOptions{Limit: -1})
	for _, c := range []struct {
		enc  string
		o    Options
		want string
	}{
		{Zstd, Options{Budget: b}, zstdTiers[1].String()},
		{Brotli, Options{Budget: b}, brotliTiers[4][2].String()},
		{Zstd, Options{Budget: b, ZstdWindow: 17, ZstdLowMemory: true}, zstdTiers[5].String()},
	} {
		s, err := Open(newRecorder(), request(t.Context(), c.enc), c.o)
		if err != nil {
			t.Fatalf("%s: %v", c.want, err)
		}
		if got := b.Stats().Streams; got[c.want] != 1 || len(got) != 1 {
			t.Errorf("%s %+v: streams %v", c.enc, c.o, got)
		}
		s.Close()
	}
}
