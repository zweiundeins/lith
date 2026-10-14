package sse

import (
	"bytes"
	"errors"
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"
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

// Frames of 17, 87, 228 and 264 KB, which windows of 2^15 (zstd; brotli's
// smallest is 2^16), 2^17, 2^18 and 2^19 hold.
var (
	small = table(100, 0)
	mid   = frame(0)
	large = table(1300, 0)
	huge  = table(1500, 0)
)

// write writes events to the stream and fails the test on an error.
func write(t *testing.T, s *Stream, events ...[]byte) {
	t.Helper()
	if err := s.Write(events...); err != nil {
		t.Fatal(err)
	}
}

// Streams that open, write frames of any size, hibernate and close in any
// order never take a tier beyond the limit, and give all of it back.
// Streams that compress nothing may take 32 KiB each beyond it. With
// Refuse, the budget turns away what it has no room for instead: the open
// streams stay within the limit, and every stream whose client accepts a
// coding compresses each write.
func TestBudgetNeverExceedsLimit(t *testing.T) {
	const limit = 24 << 20
	for _, refuse := range []bool{false, true} {
		b := NewBudget(BudgetOptions{Limit: limit, MaxWindow: 20, Refuse: refuse})
		rnd := rand.New(rand.NewPCG(1, 2))
		codings := []string{Zstd, Brotli, "br;q=1, gzip;q=0.5", Gzip, ""}
		var open []*Stream
		kinds := map[string]bool{}
		refused := 0
		for i := range 600 {
			switch r := rnd.IntN(8); {
			case len(open) > 0 && r < 1:
				k := rnd.IntN(len(open))
				open[k].Close()
				open = append(open[:k], open[k+1:]...)
			case len(open) > 0 && r < 4: // a write, which may pick, wake, shrink or lift a tier
				f := [][]byte{small, mid, mid, mid, large}[rnd.IntN(5)]
				s := open[rnd.IntN(len(open))]
				write(t, s, f)
				if refuse && s.accepts != "" && (s.encoding == "" || s.asleep) {
					t.Fatalf("step %d: a write went out uncompressed: %s", i, label(s.tier, s.encoding))
				}
			case len(open) > 0 && r < 5:
				if err := open[rnd.IntN(len(open))].Hibernate(); err != nil {
					t.Fatal(err)
				}
			default:
				s, err := Open(newRecorder(), request(t.Context(), codings[rnd.IntN(len(codings))]), Options{Budget: b, BrotliQuality: 3 + rnd.IntN(2)})
				switch {
				case refuse && errors.Is(err, ErrNoRoom):
					refused++
				case err != nil:
					t.Fatal(err)
				default:
					open = append(open, s)
				}
			}
			st := b.Stats()
			n, sum, bare := 0, int64(0), int64(0)
			for k, c := range st.Streams {
				n += c
				kinds[k] = true
			}
			for _, s := range open {
				sum += s.tier.heap + streamHeap
				if s.tier.coding == "" && !refuse {
					bare += streamHeap
				}
			}
			if st.Reserved-bare > limit {
				t.Fatalf("refuse %v, step %d: %d bytes reserved, %d of them by streams without a tier, over the limit of %d", refuse, i, st.Reserved, bare, limit)
			}
			if n != len(open) || sum != st.Reserved {
				t.Fatalf("refuse %v, step %d: the budget counts %d streams and %d bytes, the %d open streams hold %d", refuse, i, n, st.Reserved, len(open), sum)
			}
			if int64(refused) != st.Refused {
				t.Fatalf("refuse %v, step %d: %d streams refused, the budget counts %d", refuse, i, refused, st.Refused)
			}
		}
		if len(kinds) < 8 || refuse && refused == 0 {
			t.Fatalf("refuse %v: the streams held too few kinds of tier, or none was refused: %v, %d refused", refuse, kinds, refused)
		}
		for _, s := range open {
			s.Close()
		}
		if st := b.Stats(); st.Reserved != 0 || len(st.Streams) != 0 {
			t.Fatalf("refuse %v: after every stream closed: %+v", refuse, st)
		}
	}
}

// A brotli stream holds quality 3 at 2^16 until its first write, which
// picks the smallest window that holds the frame: a larger one sends no
// fewer bytes. MinWindow raises the window, the stream's BrotliWindow
// caps it, and a frame no window holds gets the smallest. Without a
// limit the stream keeps its options. Every body decodes.
func TestBudgetWindowFromFrames(t *testing.T) {
	roomy := NewBudget(BudgetOptions{Limit: 1 << 30})
	for _, c := range []struct {
		b     *Budget
		o     Options
		frame []byte
		want  string
	}{
		{roomy, Options{}, small, "br q4 2^16"},
		{roomy, Options{}, mid, "br q4 2^17"},
		{roomy, Options{}, large, "br q4 2^18"},
		{roomy, Options{}, huge, "br q4 2^19"},
		{roomy, Options{BrotliQuality: 3}, large, "br q3 2^18"},
		{roomy, Options{BrotliWindow: 18}, huge, "br q3 2^16"},
		{NewBudget(BudgetOptions{Limit: 1 << 30, MinWindow: 17}), Options{}, small, "br q4 2^17"},
		{NewBudget(BudgetOptions{Limit: 1 << 30, MaxWindow: 18}), Options{}, huge, "br q3 2^16"},
		{NewBudget(BudgetOptions{Limit: -1}), Options{}, small, "br q4 2^18"},
		{NewBudget(BudgetOptions{Limit: -1}), Options{BrotliWindow: 19}, mid, "br q4 2^19"},
	} {
		c.o.Budget = c.b
		s, w := openWith(t, Brotli, c.o)
		if n, _ := s.Tier(); c.b.limit > 0 && n != "br q3 2^16" {
			t.Errorf("before the first write: %s", n)
		}
		write(t, s, c.frame)
		if n, _ := s.Tier(); n != c.want {
			t.Errorf("%d bytes with %+v: %s, want %s", len(c.frame), c.o, n, c.want)
		}
		write(t, s, c.frame)
		s.Close()
		got, err := io.ReadAll(decoder(t, Brotli, w.bytes()))
		if err != nil || !bytes.Equal(got, append(slices.Clone(c.frame), c.frame...)) {
			t.Fatalf("%s: decodes to %d bytes: %v", c.want, len(got), err)
		}
	}
	if st := roomy.Stats(); st.Reserved != 0 || len(st.Streams) != 0 {
		t.Fatalf("after Close: %+v", st)
	}
}

// A brotli stream steps down in quality before it steps down in window: at
// quality 4 while the budget stays at most half full, then at quality 3,
// both with the window its frames need, and only when that does not fit,
// quality 3 at 2^16, which compresses each frame alone. A stream goes out
// uncompressed only when even that does not fit, and with Refuse is
// turned away instead.
func TestBudgetQualityBeforeWindow(t *testing.T) {
	// Each stream holds 32 KiB besides its tier: five at quality 4 (1,648
	// KiB each) fill half of 17,092 KiB, seven at quality 3 (1,136 each)
	// leave 900 KiB, which holds 2^16 at quality 3 but not 2^18.
	const limit = 17092 * kib
	for _, refuse := range []bool{false, true} {
		b := NewBudget(BudgetOptions{Limit: limit, Refuse: refuse})
		var got []string
		var open []*Stream
		for {
			w := newRecorder()
			s, err := Open(w, request(t.Context(), Brotli), Options{Budget: b})
			if errors.Is(err, ErrNoRoom) {
				got = append(got, "refused")
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			open = append(open, s)
			write(t, s, large)
			n, _ := s.Tier()
			got = append(got, n)
			if n == "identity" {
				if left := limit - b.Stats().Reserved + streamHeap; left >= brotliFloor.heap+streamHeap {
					t.Fatalf("uncompressed with %d KiB left", left/kib)
				}
				break
			}
		}
		last := "identity"
		if refuse {
			last = "refused"
		}
		want := slices.Concat(slices.Repeat([]string{"br q4 2^18"}, 5), slices.Repeat([]string{"br q3 2^18"}, 7), []string{"br q3 2^16", last})
		if !slices.Equal(got, want) {
			t.Errorf("refuse %v:\n got %v\nwant %v", refuse, got, want)
		}
		for _, s := range open {
			s.Close()
		}
	}
}

// A zstd stream takes the largest tier within its fair share and what is
// left. When a stream finds no room, those above the share move down at
// their next write, and the stream that waited takes the room that freed
// up at its own.
func TestBudgetZstdTiers(t *testing.T) {
	b := NewBudget(BudgetOptions{Limit: 8 << 20})
	var open []*Stream
	var got []string
	for range 6 {
		s, _ := openWith(t, Zstd, Options{Budget: b})
		open = append(open, s)
		write(t, s, mid)
		got = append(got, label(s.tier, s.encoding))
	}
	defer func() {
		for _, s := range open {
			s.Close()
		}
	}()
	// Shares of 8, 4, 2.7 and 2 MiB; then 1.6 MiB with 1 MiB left; then
	// 0.1 MiB left.
	want := "zstd 2^19, zstd 2^19, zstd 2^19, zstd 2^19, zstd 2^18 low memory, zstd asleep"
	if strings.Join(got, ", ") != want {
		t.Fatalf("tiers:\n got %s\nwant %s", strings.Join(got, ", "), want)
	}
	for _, s := range open {
		write(t, s, mid)
	}
	if got := held(b); got["zstd 2^19 low memory"] != 5 || got["zstd 2^18 low memory"] != 1 {
		t.Fatalf("after a write each: %v", got)
	}
	if r := b.Stats().Reserved; r != 6*streamHeap+5*1136*kib+880*kib {
		t.Fatalf("reserved %d", r)
	}
	// Without a limit a stream keeps its options; with ZstdLowMemory, zstd
	// gets low-memory tiers only.
	for _, c := range []struct {
		o    Options
		want string
	}{
		{Options{Budget: NewBudget(BudgetOptions{Limit: -1})}, "zstd 2^19"},
		{Options{Budget: NewBudget(BudgetOptions{Limit: -1}), ZstdLowMemory: true}, "zstd 2^19 low memory"},
		{Options{Budget: NewBudget(BudgetOptions{Limit: 1 << 30}), ZstdLowMemory: true}, "zstd 2^19 low memory"},
		{Options{Budget: NewBudget(BudgetOptions{Limit: 1 << 30, MaxWindow: 20})}, "zstd 2^20"},
	} {
		s, _ := openWith(t, Zstd, c.o)
		write(t, s, small)
		if n, _ := s.Tier(); n != c.want {
			t.Errorf("%+v: %s, want %s", c.o, n, c.want)
		}
		s.Close()
	}
	// A brotli client keeps its tiers: ZstdLowMemory is about zstd only.
	s, _ := openWith(t, Brotli, Options{Budget: NewBudget(BudgetOptions{Limit: 1 << 30}), ZstdLowMemory: true})
	write(t, s, large)
	if n, _ := s.Tier(); n != "br q4 2^18" {
		t.Errorf("brotli with ZstdLowMemory: %s", n)
	}
	s.Close()
}

// A zstd stream moves down no further than the window its frames need:
// with frames of 228 KB, 2^18. A stream that finds no room for that
// window takes the smallest, 2^15, while what is left holds it, else sends
// raw blocks, and moves up once room frees up. Streams with small frames
// share the same budget among more.
func TestBudgetZstdFloor(t *testing.T) {
	count := func(f []byte) (compressed int, tiers map[string]int) {
		b := NewBudget(BudgetOptions{Limit: 4 << 20})
		var open []*Stream
		defer func() {
			for _, s := range open {
				s.Close()
			}
		}()
		for range 8 {
			s, _ := openWith(t, Zstd, Options{Budget: b})
			open = append(open, s)
			for range 2 {
				for _, s := range open {
					write(t, s, f)
				}
			}
		}
		tiers = held(b)
		return len(open) - tiers["zstd asleep"], tiers
	}
	if n, tiers := count(large); n != 4 || tiers["zstd 2^18 low memory"] != 4 {
		t.Errorf("228 KB frames: %d compressed, %v", n, tiers)
	}
	if n, tiers := count(small); n != 6 || tiers["zstd 2^15 low memory"] != 6 {
		t.Errorf("17 KB frames: %d compressed, %v", n, tiers)
	}

	// The last resort, and the way back up.
	b := NewBudget(BudgetOptions{Limit: 2 << 20})
	b.Hold(2<<20 - streamHeap - 650*kib)
	s, w := openWith(t, Zstd, Options{Budget: b})
	write(t, s, large)
	if n, _ := s.Tier(); n != "zstd 2^15 low memory" {
		t.Fatalf("with 650 KiB left: %s", n)
	}
	b.Hold(-1 << 20)
	write(t, s, large)
	if n, _ := s.Tier(); n != "zstd 2^19 low memory" {
		t.Fatalf("after 1 MiB freed up: %s", n)
	}
	s.Close()
	got, err := io.ReadAll(decoder(t, Zstd, w.bytes()))
	if err != nil || !bytes.Equal(got, append(slices.Clone(large), large...)) {
		t.Fatalf("the stream decodes to %d bytes: %v", len(got), err)
	}
}

// Hold books memory the streams hold besides their encoders: it counts
// against the limit and leaves the encoders less room, but is never
// refused. Once it alone takes the limit, streams go out uncompressed.
func TestBudgetHold(t *testing.T) {
	b := NewBudget(BudgetOptions{Limit: 8 << 20})
	b.Hold(6 << 20)
	if st := b.Stats(); st.Frames != 6<<20 || st.Reserved != 6<<20 {
		t.Fatalf("after Hold: %+v", st)
	}
	z1, _ := openWith(t, Zstd, Options{Budget: b})
	write(t, z1, mid)
	z2, _ := openWith(t, Zstd, Options{Budget: b})
	write(t, z2, mid)
	if n1, n2 := label(z1.tier, Zstd), label(z2.tier, Zstd); n1 != "zstd 2^19" || n2 != "zstd asleep" {
		t.Fatalf("with 2 MiB besides frames: %s, %s", n1, n2)
	}
	b.Hold(-2 << 20)
	write(t, z1, mid)
	write(t, z2, mid)
	if got := held(b); got["zstd 2^19"] != 2 {
		t.Fatalf("after 2 MiB went back: %v", got)
	}
	b.Hold(20 << 20)
	br, w := openWith(t, Brotli, Options{Budget: b})
	if br.Encoding() != "" || w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("a brotli stream with frames over the limit: %q", br.Encoding())
	}
	write(t, z1, mid)
	write(t, z2, mid)
	if got := held(b); got["zstd asleep"] != 2 {
		t.Fatalf("zstd streams with frames over the limit: %v", got)
	}
	for _, s := range []*Stream{z1, z2, br} {
		s.Close()
	}
	b.Hold(-24 << 20)
	if st := b.Stats(); st.Reserved != 0 || st.Frames != 0 {
		t.Fatalf("after Close: %+v", st)
	}
}

// A brotli stream whose window is below its frames, or that went out
// uncompressed for want of room, learns through Upgrade when a new stream
// would get a window that holds them: the budget closes its channel once
// room frees up, in the order the streams called Upgrade, one every
// reopenEvery. Streams whose window holds their frames, zstd streams and
// streams without a budget are never asked.
func TestBudgetUpgrade(t *testing.T) {
	asked := func(s *Stream) bool {
		select {
		case <-s.Upgrade():
			return true
		default:
			return false
		}
	}
	eventually := func(s *Stream) time.Time {
		t.Helper()
		select {
		case <-s.Upgrade():
			return time.Now()
		case <-time.After(5 * time.Second):
			t.Fatal("not asked within 5 s")
		}
		return time.Time{}
	}
	const every = 50 * time.Millisecond
	b := NewBudget(BudgetOptions{Limit: 8 << 20})
	b.reopenEvery = every
	// After three streams at 2^16, 200 KiB are left: not enough for 2^18
	// at quality 3, nor for a fourth stream.
	fill := int64(8<<20 - 3*(brotliFloor.heap+streamHeap) - 200*kib)
	b.Hold(fill)
	var below []*Stream
	for range 3 {
		s, _ := openWith(t, Brotli, Options{Budget: b})
		below = append(below, s)
	}
	id, _ := openWith(t, Brotli, Options{Budget: b})
	z, _ := openWith(t, Zstd, Options{Budget: b})
	for _, s := range append(below, id, z) {
		write(t, s, large)
	}
	for _, s := range below {
		if n, _ := s.Tier(); n != "br q3 2^16" {
			t.Fatalf("a stream that found no room for 2^18: %s", n)
		}
	}
	if n, _ := id.Tier(); n != "identity" {
		t.Fatalf("a stream without room: %s", n)
	}
	for _, s := range append(below, id, z) {
		if asked(s) {
			t.Fatal("asked to open again without room")
		}
	}
	// Room for one better tier: the first is asked, at once.
	b.Hold(-600 * kib)
	if !asked(below[0]) || asked(below[1]) || asked(below[2]) || asked(id) {
		t.Fatal("after 600 KiB freed up, the first stream is not the one asked")
	}
	// A stream that writes on before its handler ends it is not listed
	// again: its channel is closed once.
	write(t, below[0], huge)
	b.mu.Lock()
	again := slices.Contains(b.movers, below[0].up)
	b.mu.Unlock()
	if again {
		t.Fatal("a stream asked to open again is listed again")
	}
	below[0].Close()
	// Room for all: one more every reopenEvery.
	b.Hold(-fill + 600*kib)
	if asked(below[1]) {
		t.Fatal("a second stream asked within reopenEvery")
	}
	var at []time.Time
	for _, s := range []*Stream{below[1], below[2], id} {
		at = append(at, eventually(s))
	}
	for i := 1; i < len(at); i++ {
		if d := at[i].Sub(at[i-1]); d < every*9/10 {
			t.Errorf("asked %v after the one before", d)
		}
	}
	if st := b.Stats(); st.Reopened != 4 {
		t.Fatalf("reopened %d, want 4", st.Reopened)
	}
	// A stream whose window holds its frames is not asked, however much
	// room there is, and neither is a zstd stream.
	ok, _ := openWith(t, Brotli, Options{Budget: b})
	write(t, ok, large)
	time.Sleep(2 * every)
	if asked(ok) || asked(z) {
		t.Fatal("a stream at 2^18 or a zstd stream asked")
	}
	for _, s := range append(below[1:], id, z, ok) {
		s.Close()
	}
	b.mu.Lock()
	movers := len(b.movers)
	b.mu.Unlock()
	if st := b.Stats(); st.Reserved != 0 || movers != 0 {
		t.Fatalf("after Close: %+v, %d movers", st, movers)
	}
	s, _ := openWith(t, Brotli, Options{})
	write(t, s, large)
	if asked(s) {
		t.Fatal("a stream without a budget asked")
	}
	s.Close()
}

// A brotli stream that closes before its first write ends with an empty
// brotli body; one that closes with events picks its tier for them.
func TestBrotliCloseBeforeWriting(t *testing.T) {
	b := NewBudget(BudgetOptions{Limit: 1 << 30})
	for _, events := range [][][]byte{nil, {large}} {
		s, w := openWith(t, Brotli, Options{Budget: b})
		if err := s.CloseWith(events...); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(decoder(t, Brotli, w.bytes()))
		if err != nil || !bytes.Equal(got, bytes.Join(events, nil)) {
			t.Fatalf("%d events: decodes to %d bytes: %v", len(events), len(got), err)
		}
	}
	if st := b.Stats(); st.Reserved != 0 {
		t.Fatalf("after Close: %+v", st)
	}
}

// Streams that took large tiers while few were open move down at their
// next write once a stream finds no room, and that stream gets room at its
// next write. Every body still decodes to what was written.
func TestBudgetShrinks(t *testing.T) {
	b := NewBudget(BudgetOptions{Limit: 4 << 20, MinWindow: 17})
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
	// Shares of 4 and 2 MiB; the third, 1.3 MiB, finds 0.5 MiB left.
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

// A lower limit moves zstd streams down at their next write, no lower than
// their frames' window while the share allows, and those it leaves no room
// for send raw frames; every body still decodes.
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
	if got, st := held(b), b.Stats(); got["zstd 2^17 low memory"] != 2 || got["zstd asleep"] != 2 || st.Reserved > 2<<20 {
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
// it wakes: here the largest, which it did not get when it first wrote.
func TestBudgetWakeTakesTheShareOfTheMoment(t *testing.T) {
	b := NewBudget(BudgetOptions{Limit: 3 << 20})
	first, _ := openWith(t, Zstd, Options{Budget: b})
	s, w := openWith(t, Zstd, Options{Budget: b})
	write(t, s, frame(1))
	if got := label(s.tier, s.encoding); got != "zstd 2^19 low memory" {
		t.Fatalf("first wrote with %s", got)
	}
	first.Close()
	s.Hibernate()
	write(t, s, frame(2))
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
	b := NewBudget(BudgetOptions{Limit: 1 << 20, MaxWindow: 15})
	first, _ := openWith(t, Zstd, Options{Budget: b})
	write(t, first, frame(0))
	if got := label(first.tier, first.encoding); got != "zstd 2^15 low memory" {
		t.Fatalf("the first stream got %s", got)
	}

	br, w := openWith(t, Brotli, Options{Budget: b})
	if br.Encoding() != "" || w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("a brotli stream without room: %q", br.Encoding())
	}
	br.Close()

	z, w := openWith(t, Zstd, Options{Budget: b})
	var want []byte
	for i := range 3 {
		write(t, z, frame(i))
		want = append(want, frame(i)...)
	}
	if !z.asleep {
		t.Fatalf("a zstd stream without room: %s", label(z.tier, z.encoding))
	}
	if _, wire := z.Sent(); wire < int64(len(want)) {
		t.Fatalf("%d bytes on the wire for %d bytes of events: compressed without room", wire, len(want))
	}
	// Room frees up: the next write compresses.
	first.Close()
	write(t, z, frame(3))
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

	refusing := NewBudget(BudgetOptions{Limit: 1 << 20, MaxWindow: 15, Refuse: true})
	keep, _ := openWith(t, Zstd, Options{Budget: refusing})
	write(t, keep, frame(0))
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
	// A stream that compresses nothing needs room for its 32 KiB only.
	id, _ := openWith(t, "identity", Options{Budget: refusing})
	id.Close()
}

// With Refuse, a stream that compresses nothing is turned away when the
// budget has no room for its 32 KiB, as one whose client accepts no coding
// or one with NoCompression; without Refuse it is let in beyond the limit.
// Stats count the refusals.
func TestBudgetRefusesIdentity(t *testing.T) {
	const limit = 1 << 20
	for _, refuse := range []bool{false, true} {
		b := NewBudget(BudgetOptions{Limit: limit, Refuse: refuse})
		b.Hold(limit - 16*kib)
		for _, c := range []struct {
			coding string
			o      Options
		}{
			{"identity", Options{}},
			{Zstd, Options{NoCompression: true}},
		} {
			c.o.Budget = b
			w := newRecorder()
			s, err := Open(w, request(t.Context(), c.coding), c.o)
			if !refuse {
				if err != nil {
					t.Fatalf("without Refuse, %s with %+v: %v", c.coding, c.o, err)
				}
				if n, _ := s.Tier(); n != "identity" {
					t.Fatalf("without Refuse, %s with %+v: %s", c.coding, c.o, n)
				}
				s.Close()
				continue
			}
			if !errors.Is(err, ErrNoRoom) {
				t.Fatalf("with Refuse, %s with %+v and 16 KiB left: %v", c.coding, c.o, err)
			}
			if len(w.Header()) != 0 || len(w.bytes()) != 0 {
				t.Fatalf("a refused stream wrote %v, %q", w.Header(), w.bytes())
			}
		}
		want := int64(0)
		if refuse {
			want = 2
		}
		if st := b.Stats(); st.Refused != want || st.Reserved != limit-16*kib || len(st.Streams) != 0 {
			t.Fatalf("refuse %v: %+v", refuse, st)
		}
		// Room for 32 KiB: the stream gets in.
		b.Hold(-16 * kib)
		s, _ := openWith(t, "identity", Options{Budget: b})
		if st := b.Stats(); st.Reserved != limit || st.Streams["identity"] != 1 {
			t.Fatalf("refuse %v, with room: %+v", refuse, st)
		}
		s.Close()
	}
}

// With Refuse, a zstd stream books its smallest tier from Open, while it
// has no encoder too, so that it never sends raw blocks: a second stream
// that would have to is turned away. It keeps that tier booked while it
// hibernates and takes it when nothing else fits, even after Hold or a
// lower limit took the budget past what it holds.
func TestBudgetRefuseKeepsZstdCompressed(t *testing.T) {
	b := NewBudget(BudgetOptions{Limit: 1 << 20, MaxWindow: 15, Refuse: true})
	z, w := openWith(t, Zstd, Options{Budget: b})
	if st := b.Stats(); st.Streams["zstd asleep"] != 1 || st.Reserved != zstdFloor.heap+streamHeap {
		t.Fatalf("before its first write: %+v", st)
	}
	// Without Refuse this one got in too and sent raw blocks (TestBudgetFull).
	if _, err := Open(newRecorder(), request(t.Context(), Zstd), Options{Budget: b}); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("a second zstd stream: %v", err)
	}
	var want []byte
	step := func(what string) {
		t.Helper()
		f := frame(len(want))
		write(t, z, f)
		want = append(want, f...)
		if z.asleep || label(z.tier, z.encoding) != "zstd 2^15 low memory" {
			t.Fatalf("%s: asleep %v, %s", what, z.asleep, label(z.tier, z.encoding))
		}
		if st := b.Stats(); st.Reserved-st.Frames != zstdFloor.heap+streamHeap {
			t.Fatalf("%s: %+v", what, st)
		}
	}
	step("the first write")
	if err := z.Hibernate(); err != nil {
		t.Fatal(err)
	}
	if n, heap := z.Tier(); n != "zstd asleep" || heap != zstdFloor.heap {
		t.Fatalf("hibernating: %s, %d", n, heap)
	}
	if _, err := Open(newRecorder(), request(t.Context(), Zstd), Options{Budget: b}); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("a zstd stream while the first hibernates: %v", err)
	}
	b.Hold(1 << 20)
	step("waking past the limit")
	if err := z.Hibernate(); err != nil {
		t.Fatal(err)
	}
	step("waking again")
	b.Hold(-1 << 20)
	z.Close()
	got, err := io.ReadAll(decoder(t, Zstd, w.bytes()))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the stream decodes to %d bytes of %d: %v", len(got), len(want), err)
	}

	// A lower limit moves a stream down to its smallest tier, not to none.
	b = NewBudget(BudgetOptions{Limit: 8 << 20, Refuse: true})
	z, w = openWith(t, Zstd, Options{Budget: b})
	write(t, z, mid)
	if n, _ := z.Tier(); n != "zstd 2^19" {
		t.Fatalf("with 8 MiB: %s", n)
	}
	b.SetLimit(256 * kib)
	write(t, z, mid)
	if n, _ := z.Tier(); z.asleep || n != "zstd 2^15 low memory" {
		t.Fatalf("after SetLimit(256 KiB): asleep %v, %s", z.asleep, n)
	}
	z.Close()
	got, err = io.ReadAll(decoder(t, Zstd, w.bytes()))
	if err != nil || !bytes.Equal(got, append(slices.Clone(mid), mid...)) {
		t.Fatalf("after SetLimit, the stream decodes to %d bytes: %v", len(got), err)
	}
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
		if s.enc != nil || s.buf.Cap() != 0 || b.Stats().Reserved != streamHeap || held(b)["zstd asleep"] != 1 {
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

// Tier names what a stream holds as its budget counts it, and the heap the
// budget reserved for it; without a budget, the stream's own settings.
// Budget is the budget the stream shares.
func TestStreamTier(t *testing.T) {
	b := NewBudget(BudgetOptions{Limit: 8 << 20, MinWindow: 18, MaxWindow: 19})
	var open []*Stream
	for _, c := range []string{Zstd, Zstd, Zstd, Brotli, Gzip, ""} {
		s, _ := openWith(t, c, Options{Budget: b})
		open = append(open, s)
	}
	agree := func(when string) {
		t.Helper()
		names := map[string]int{}
		var heap int64
		for _, s := range open {
			n, h := s.Tier()
			names[n]++
			heap += h
			if s.Budget() != b {
				t.Fatalf("%s: a stream's Budget is not its budget", when)
			}
		}
		st := b.Stats()
		if !maps.Equal(names, st.Streams) || heap+int64(len(open))*streamHeap != st.Reserved {
			t.Fatalf("%s: the streams name %v and hold %d bytes, the budget counts %v and %d", when, names, heap, st.Streams, st.Reserved)
		}
	}
	agree("open")
	// Before its first write a brotli stream holds the least; then the
	// window its frames need, at least MinWindow.
	if n, h := open[3].Tier(); n != "br q3 2^16" || h != 720*kib {
		t.Errorf("brotli before its first write: %s, %d bytes", n, h)
	}
	write(t, open[3], small)
	if n, h := open[3].Tier(); n != "br q4 2^18" || h != 1616*kib {
		t.Errorf("brotli: %s, %d bytes", n, h)
	}
	if n, _ := open[5].Tier(); n != "identity" {
		t.Errorf("a stream without a coding: %s", n)
	}
	if n, _ := open[0].Tier(); n != "zstd asleep" {
		t.Errorf("zstd before its first write: %s", n)
	}
	// A fair share of 1.6 MiB among five streams that compress.
	write(t, open[0], Comment("x"))
	if n, _ := open[0].Tier(); n != "zstd 2^19 low memory" {
		t.Errorf("zstd: %s", n)
	}
	agree("written")
	if err := open[0].Hibernate(); err != nil {
		t.Fatal(err)
	}
	if n, h := open[0].Tier(); n != "zstd asleep" || h != 0 {
		t.Errorf("hibernating: %s, %d bytes", n, h)
	}
	agree("one asleep")
	for _, s := range open {
		s.Close()
	}

	for _, c := range []struct {
		enc  string
		o    Options
		want string
	}{
		{Zstd, Options{}, "zstd 2^19"},
		{Zstd, Options{ZstdWindow: 17, ZstdLowMemory: true}, "zstd 2^17 low memory"},
		{Brotli, Options{}, "br q4 2^18"},
		{Brotli, Options{BrotliQuality: 3, BrotliWindow: 16}, "br q3 2^16"},
		{Gzip, Options{}, "gzip"},
		{"", Options{}, "identity"},
		{Zstd, Options{NoCompression: true}, "identity"},
	} {
		s, _ := openWith(t, c.enc, c.o)
		if n, h := s.Tier(); n != c.want || h != 0 || s.Budget() != nil {
			t.Errorf("%s %+v without a budget: %s, %d bytes", c.enc, c.o, n, h)
		}
		if c.want == "zstd 2^19" {
			if err := s.Hibernate(); err != nil {
				t.Fatal(err)
			}
			if n, _ := s.Tier(); n != "zstd asleep" {
				t.Errorf("hibernating without a budget: %s", n)
			}
		}
		s.Close()
	}
}
