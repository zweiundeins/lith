package sse

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// A slow stream without a budget compresses zstd at its default level: on
// frames that change five rows of 500 it sends fewer bytes, and the body
// decodes. Its brotli and a ZstdLevel above the default stay as set.
func TestSlowStreamWithoutBudget(t *testing.T) {
	sent := map[bool]int64{}
	for _, slow := range []bool{false, true} {
		s, w := openWith(t, Zstd, Options{Slow: slow})
		var all [][]byte
		for i := range 20 {
			f := frame(i)
			all = append(all, f)
			write(t, s, f)
		}
		_, sent[slow] = s.Sent()
		name, _ := s.Tier()
		if want := map[bool]string{false: "zstd 2^19", true: "zstd default 2^19"}[slow]; name != want {
			t.Errorf("slow %v: %s, want %s", slow, name, want)
		}
		s.Close()
		got, err := io.ReadAll(decoder(t, Zstd, w.bytes()))
		if err != nil || !bytes.Equal(got, bytes.Join(all, nil)) {
			t.Fatalf("slow %v: the body does not decode to the frames: %v", slow, err)
		}
	}
	if sent[true] > sent[false]*3/4 {
		t.Errorf("a slow stream sent %d bytes, a fast one %d", sent[true], sent[false])
	}
	s, _ := openWith(t, Zstd, Options{Slow: true, ZstdLevel: zstd.SpeedBetterCompression})
	if s.zstd.level != zstd.SpeedBetterCompression {
		t.Errorf("a slow stream lowered ZstdLevel to %v", s.zstd.level)
	}
	s.Close()
	s, _ = openWith(t, Brotli, Options{Slow: true})
	if name, _ := s.Tier(); name != "br q4 2^18" {
		t.Errorf("a slow brotli stream: %s", name)
	}
	s.Close()
}

// With room in its budget, a slow zstd stream takes the tier a stream would
// take at zstd's default level; when that does not fit its share, the
// fastest level's. A slow brotli stream keeps quality 4 while the budget
// has room for it, beyond half full.
func TestBudgetSlowStreams(t *testing.T) {
	dense := zstdDense[tier{coding: Zstd, window: 19}]
	for _, c := range []struct {
		name  string
		limit int64
		o     Options
		want  string
	}{
		{"room", 1 << 30, Options{Slow: true}, "zstd default 2^19"},
		{"room, low memory", 1 << 30, Options{Slow: true, ZstdLowMemory: true}, "zstd default 2^19 low memory"},
		{"no limit", -1, Options{Slow: true}, "zstd default 2^19"},
		{"room for the denser tier alone", dense + streamHeap, Options{Slow: true}, "zstd default 2^19"},
		{"room for the fastest's", dense - kib + streamHeap, Options{Slow: true}, "zstd 2^19"},
		{"not slow", 1 << 30, Options{}, "zstd 2^19"},
	} {
		c.o.Budget = NewBudget(BudgetOptions{Limit: c.limit})
		s, w := openWith(t, Zstd, c.o)
		write(t, s, mid)
		write(t, s, mid)
		name, heap := s.Tier()
		if name != c.want {
			t.Errorf("%s: %s, want %s", c.name, name, c.want)
		}
		if st := c.o.Budget.Stats(); st.Streams[name] != 1 || st.Reserved != heap+streamHeap {
			t.Errorf("%s: the budget counts %v, %d bytes; the stream holds %s, %d", c.name, st.Streams, st.Reserved, name, heap)
		}
		if name == "zstd default 2^19" && s.zstd.level != zstd.SpeedDefault {
			t.Errorf("%s: the encoder runs at %v", c.name, s.zstd.level)
		}
		s.Close()
		got, err := io.ReadAll(decoder(t, Zstd, w.bytes()))
		if err != nil || !bytes.Equal(got, append(bytes.Clone(mid), mid...)) {
			t.Fatalf("%s: the body does not decode to the frames: %v", c.name, err)
		}
	}

	// A budget a little over half full: a brotli stream takes quality 3, a
	// slow one 4.
	b := NewBudget(BudgetOptions{Limit: 16 << 20})
	b.Hold(8 << 20)
	for _, slow := range []bool{false, true} {
		s, _ := openWith(t, Brotli, Options{Budget: b, Slow: slow})
		write(t, s, large)
		if name, _ := s.Tier(); name != map[bool]string{false: "br q3 2^18", true: "br q4 2^18"}[slow] {
			t.Errorf("slow %v, a budget half full: %s", slow, name)
		}
		s.Close()
	}
	// Without room for quality 4, a slow stream takes 3.
	b.Hold(7<<20 - 512*kib)
	s, _ := openWith(t, Brotli, Options{Budget: b, Slow: true})
	write(t, s, large)
	if name, _ := s.Tier(); name != "br q3 2^18" {
		t.Errorf("slow, little room: %s", name)
	}
	s.Close()
}

// When a lower limit leaves a slow zstd stream's denser tier above its
// share, the stream moves to the fastest level's tier at its next write,
// and the body still decodes.
func TestBudgetSlowStreamShrinks(t *testing.T) {
	b := NewBudget(BudgetOptions{Limit: 1 << 30})
	s, w := openWith(t, Zstd, Options{Budget: b, Slow: true})
	write(t, s, mid)
	if name, _ := s.Tier(); name != "zstd default 2^19" {
		t.Fatalf("with room: %s", name)
	}
	b.SetLimit(2 << 20)
	write(t, s, mid)
	if name, _ := s.Tier(); name != "zstd 2^19" {
		t.Errorf("after the limit fell to 2 MiB: %s", name)
	}
	if st := b.Stats(); st.Reserved > 2<<20 {
		t.Errorf("%d bytes reserved", st.Reserved)
	}
	s.Close()
	got, err := io.ReadAll(decoder(t, Zstd, w.bytes()))
	if err != nil || !bytes.Equal(got, append(bytes.Clone(mid), mid...)) {
		t.Fatalf("the body does not decode to the frames: %v", err)
	}
}

// Slow and other streams that open, write, hibernate and close in any
// order stay within the limit, as the budget counts them, and give all of
// it back; with Refuse, every stream let in compresses each write.
func TestBudgetSlowNeverExceedsLimit(t *testing.T) {
	const limit = 24 << 20
	for _, refuse := range []bool{false, true} {
		b := NewBudget(BudgetOptions{Limit: limit, MaxWindow: 20, Refuse: refuse})
		rnd := rand.New(rand.NewPCG(3, 4))
		codings := []string{Zstd, Zstd, Brotli, Gzip, ""}
		var open []*Stream
		kinds := map[string]bool{}
		for i := range 600 {
			switch r := rnd.IntN(8); {
			case len(open) > 0 && r < 2:
				k := rnd.IntN(len(open))
				open[k].Close()
				open = append(open[:k], open[k+1:]...)
			case len(open) > 0 && r < 5:
				s := open[rnd.IntN(len(open))]
				write(t, s, [][]byte{small, mid, mid, large}[rnd.IntN(4)])
				if refuse && s.accepts != "" && (s.encoding == "" || s.asleep) {
					t.Fatalf("step %d: a write went out uncompressed: %s", i, label(s.tier, s.encoding))
				}
			case len(open) > 0 && r < 6:
				if err := open[rnd.IntN(len(open))].Hibernate(); err != nil {
					t.Fatal(err)
				}
			default:
				o := Options{Budget: b, Slow: rnd.IntN(3) > 0, Frame: []int{0, len(small), len(mid)}[rnd.IntN(3)]}
				s, err := Open(newRecorder(), request(t.Context(), codings[rnd.IntN(len(codings))]), o)
				switch {
				case refuse && errors.Is(err, ErrNoRoom):
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
			if st.Reserved-bare > limit || n != len(open) || sum != st.Reserved {
				t.Fatalf("refuse %v, step %d: the budget counts %d streams and %d bytes (limit %d), the %d open streams hold %d", refuse, i, n, st.Reserved, limit, len(open), sum)
			}
		}
		dense := 0
		for k := range kinds {
			if strings.HasPrefix(k, "zstd default") {
				dense++
			}
		}
		if dense == 0 {
			t.Errorf("refuse %v: the slow streams held %d denser tiers: %v", refuse, dense, kinds)
		}
		for _, s := range open {
			s.Close()
		}
		if st := b.Stats(); st.Reserved != 0 || len(st.Streams) != 0 {
			t.Fatalf("refuse %v: after every stream closed: %+v", refuse, st)
		}
	}
}
