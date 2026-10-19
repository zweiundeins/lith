package live

import (
	"context"
	"database/sql"
	"math/rand/v2"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// Each Changed is a commit with the next number, and a topic's version is
// the number of the last commit that touched it. MarkAll touches every
// topic.
func TestVersions(t *testing.T) {
	h := New(Options{})
	if h.Seq() != 0 || h.Version("a") != 0 || h.Version() != 0 {
		t.Fatalf("a new hub: seq %d, version %d", h.Seq(), h.Version("a"))
	}
	h.Changed([]string{"a", "b"})
	h.Changed([]string{"b"})
	h.Changed(nil)
	if h.Seq() != 3 {
		t.Fatalf("seq %d after three commits", h.Seq())
	}
	for _, c := range []struct {
		topics []string
		want   uint64
	}{{[]string{"a"}, 1}, {[]string{"b"}, 2}, {[]string{"a", "b"}, 2}, {[]string{"c"}, 0}, {[]string{"c", "a"}, 1}} {
		if got := h.Version(c.topics...); got != c.want {
			t.Errorf("Version(%q) = %d, want %d", c.topics, got, c.want)
		}
	}
	h.MarkAll()
	h.Changed([]string{"a"})
	if h.Seq() != 5 || h.Version("a") != 5 || h.Version("b") != 4 || h.Version("c") != 4 || h.Version() != 4 {
		t.Fatalf("after MarkAll: seq %d, a %d, b %d, c %d", h.Seq(), h.Version("a"), h.Version("b"), h.Version("c"))
	}
}

// A hub keeps the versions of at least Options.Versions topics, those
// changed last, and at most twice as many. A topic it no longer keeps
// reads as changed later than it did, never earlier.
func TestVersionsForgetOnlyUpward(t *testing.T) {
	const keep = 64
	h := New(Options{Versions: keep})
	last := map[string]uint64{}
	var recent []string // the topics, the one changed last at the end
	r := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		topics := make([]string, 1+r.IntN(4))
		for i := range topics {
			topics[i] = "t" + strconv.Itoa(r.IntN(1000))
		}
		h.Changed(topics)
		for _, tp := range topics {
			last[tp] = h.Seq()
			recent = append(recent, tp)
		}
		if n := len(h.versions) + len(h.older); n > 2*keep {
			t.Fatalf("%d versions kept, at most %d", n, 2*keep)
		}
	}
	for tp, want := range last {
		if got := h.Version(tp); got < want {
			t.Fatalf("Version(%s) = %d, but it changed at %d", tp, got, want)
		}
	}
	seen := map[string]bool{}
	for i := len(recent) - 1; i >= 0 && len(seen) < keep; i-- {
		tp := recent[i]
		if seen[tp] {
			continue
		}
		seen[tp] = true
		if got := h.Version(tp); got != last[tp] {
			t.Fatalf("Version(%s) = %d, want %d: one of the %d changed last", tp, got, last[tp], keep)
		}
	}

	h = New(Options{Versions: -1})
	h.Changed([]string{"a"})
	h.Changed([]string{"b"})
	if h.Version("a") != 2 || len(h.versions)+len(h.older) != 0 {
		t.Fatalf("without versions: Version(a) = %d, %d kept", h.Version("a"), len(h.versions)+len(h.older))
	}
}

// Seq(ctx) in a Render is a number every commit up to which is in the
// render's snapshot: the hub reads it before the read transaction begins.
// A commit after the snapshot does not move it, so a value cached under it
// is never older than its number says.
func TestSeqInRenderIsBeforeItsSnapshot(t *testing.T) {
	write, read := testDB(t, 4)
	h := start(t, Options{Read: read, Workers: 1, Tick: 5 * time.Millisecond})
	commit := func() {
		increment(t, write)
		h.Changed([]string{"counter"})
	}
	for range 3 {
		commit()
	}
	type seen struct {
		n                  int
		before, after, ver uint64
	}
	results := make(chan seen, 4)
	var renders atomic.Int64
	v := text("v", []string{"counter"}, func(ctx context.Context, tx *sql.Tx) (string, error) {
		var s seen
		var err error
		if s.n, err = count(ctx, tx); err != nil {
			return "", err
		}
		s.before = Seq(ctx)
		if renders.Add(1) == 1 {
			commit() // after the snapshot
		}
		s.after, s.ver = Seq(ctx), h.Version("counter")
		results <- s
		return strconv.Itoa(s.n), nil
	})
	serve(h, Spec{Views: []View{v}}, "")
	for i, want := range []seen{{n: 3, before: 3, after: 3, ver: 4}, {n: 4, before: 4, after: 4, ver: 4}} {
		select {
		case s := <-results:
			if s != want {
				t.Fatalf("render %d saw %+v, want %+v", i+1, s, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no render %d", i+1)
		}
	}
	if Seq(context.Background()) != 0 {
		t.Fatal("Seq outside a Render is not 0")
	}
}

// While commits land all the time, every Render's snapshot holds at least
// the commits up to Seq(ctx): a cache keyed by it may compute again, but
// never keeps an old value under a newer number.
func TestSeqUnderCommits(t *testing.T) {
	write, read := testDB(t, 8)
	h := start(t, Options{Read: read, Workers: 4, Tick: time.Millisecond})
	var bad, renders atomic.Int64
	vs := make([]View, 8)
	for i := range vs {
		vs[i] = text("v"+strconv.Itoa(i), []string{"counter"}, func(ctx context.Context, tx *sql.Tx) (string, error) {
			seq := Seq(ctx)
			n, err := count(ctx, tx)
			if err == nil && uint64(n) < seq {
				bad.Add(1)
			}
			renders.Add(1)
			return strconv.Itoa(n), err
		})
	}
	serve(h, Spec{Views: vs}, "")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	for ctx.Err() == nil {
		increment(t, write)
		h.Changed([]string{"counter"})
	}
	if bad.Load() > 0 {
		t.Fatalf("%d of %d renders saw fewer commits than Seq(ctx)", bad.Load(), renders.Load())
	}
	if renders.Load() < 20 {
		t.Fatalf("only %d renders", renders.Load())
	}
}
