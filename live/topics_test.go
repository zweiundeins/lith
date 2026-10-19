package live

import (
	"context"
	"database/sql"
	"math/rand/v2"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// treeHub is a hub with a view on each topic, keyed by it, which one
// stream watches; Run is not called and no view is dirty.
func treeHub(o Options, topics ...string) *Hub {
	h := New(o)
	s := &stream{wake: make(chan struct{}, 1)}
	h.mu.Lock()
	for _, tp := range topics {
		h.watch(s, View{Key: tp, Topics: []string{tp}, Render: func(context.Context, *sql.Tx) (any, error) { return nil, nil }})
	}
	h.mu.Unlock()
	dirtyKeys(h)
	return h
}

// dirtyKeys returns the keys of the dirty views, sorted, and cleans them.
func dirtyKeys(h *Hub) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var keys []string
	for _, v := range h.dirty {
		keys = append(keys, v.key)
		v.dirty = false
	}
	clear(h.dirty)
	h.dirty = h.dirty[:0]
	slices.Sort(keys)
	return keys
}

// A commit to a topic touches the topics above it, and not those below;
// one to a topic with :* touches everything below it as well.
func TestTopicTree(t *testing.T) {
	h := treeHub(Options{}, "a", "a:b", "a:b:c", "a:b:c:d", "a:x", "ab", "b")
	for _, c := range []struct {
		topics []string
		want   []string
	}{
		{[]string{"a:b:c"}, []string{"a", "a:b", "a:b:c"}},
		{[]string{"a:b:c:d:e"}, []string{"a", "a:b", "a:b:c", "a:b:c:d"}},
		{[]string{"a"}, []string{"a"}},
		{[]string{"a:y:z"}, []string{"a"}},
		{[]string{"a:b:*"}, []string{"a", "a:b", "a:b:c", "a:b:c:d"}},
		{[]string{"a:b:c", "a:b:*"}, []string{"a", "a:b", "a:b:c", "a:b:c:d"}},
		{[]string{"a:*"}, []string{"a", "a:b", "a:b:c", "a:b:c:d", "a:x"}},
		{[]string{"a:q:*"}, []string{"a"}},
		{[]string{"*"}, []string{"a", "a:b", "a:b:c", "a:b:c:d", "a:x", "ab", "b"}},
		{[]string{"c:d"}, nil},
	} {
		h.Changed(c.topics)
		if got := dirtyKeys(h); !slices.Equal(got, c.want) {
			t.Errorf("Changed(%q) marked %q, want %q", c.topics, got, c.want)
		}
	}
}

// A commit to a topic renders the views of the topics above it.
func TestTopicTreeRenders(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	var n atomic.Int64
	view := func(key string) View {
		return text(key, []string{key}, func(context.Context, *sql.Tx) (string, error) {
			return strconv.FormatInt(n.Add(1), 10), nil
		})
	}
	c := serve(h, Spec{Views: []View{view("a"), view("a:b"), view("a:b:c:d"), view("a:x")}}, "")
	waitFor(t, "the first frames", func() bool { return h.Stats().Renders == 4 })
	h.Changed([]string{"a:b:c"})
	waitFor(t, "a and a:b", func() bool {
		r := h.Stats().RendersByKey
		return r["a"] == 2 && r["a:b"] == 2
	})
	h.Changed([]string{"a:b:*"})
	waitFor(t, "a:b:c:d", func() bool { return h.Stats().RendersByKey["a:b:c:d"] == 2 })
	r := h.Stats().RendersByKey
	if r["a"] != 3 || r["a:b"] != 3 || r["a:x"] != 1 {
		t.Fatalf("renders %v", r)
	}
	c.leave(t)
}

// One commit that names two topics under a:b marks a:b and a once.
func TestTopicTreeDedupes(t *testing.T) {
	h := treeHub(Options{}, "a", "a:b")
	h.Changed([]string{"x"})
	h.Changed([]string{"a:b:1", "a:b:2", "a:b"})
	if h.Seq() != 2 {
		t.Fatalf("seq %d after two commits", h.Seq())
	}
	h.mu.Lock()
	n := len(h.dirty)
	h.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d views on the dirty list, want 2", n)
	}
}

// A view that declares a coarse topic with Depends renders for commits
// below it, and for a :* above it, and not for one on a sibling.
func TestTopicTreeDepends(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	var space atomic.Value
	space.Store("A")
	v := text("tab", []string{"tab"}, func(ctx context.Context, _ *sql.Tx) (string, error) {
		s := space.Load().(string)
		Depends(ctx, "src:"+s)
		return s, nil
	})
	c := serve(h, Spec{Views: []View{v}}, "")
	renders := func() int64 { return h.Stats().RendersByKey["tab"] }
	waitFor(t, "the first frame", func() bool { return renders() == 1 })
	for i, tp := range []string{"src:A:7", "src:A:7:*", "src:*", "*"} {
		h.Changed([]string{tp})
		waitFor(t, tp, func() bool { return renders() == int64(i+2) })
	}
	h.Changed([]string{"src:B:*", "src:B", "src:AB", "tabs"})
	time.Sleep(30 * time.Millisecond)
	if n := renders(); n != 5 {
		t.Fatalf("commits beside src:A rendered the view %d times", n-5)
	}
	c.leave(t)
}

// Once nobody watches, the tree is empty: the nodes above the topics go
// with them.
func TestTopicTreeEmpties(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	var route atomic.Value
	route.Store("x:1:2")
	nav := text("nav", []string{"tab:1"}, func(ctx context.Context, _ *sql.Tx) (string, error) {
		r := route.Load().(string)
		Depends(ctx, r, r+":c", "src:A")
		return r, nil
	})
	views := []View{nav, text("v", []string{"src:A:1", "src:A:2", "src", "src:B:3:4"}, func(context.Context, *sql.Tx) (string, error) { return "v", nil })}
	c1 := serve(h, Spec{ID: "t1", Views: views}, "")
	c2 := serve(h, Spec{ID: "t2", Views: views[1:]}, "")
	waitFor(t, "the first frames", func() bool { return c1.has(`<p id="nav">x:1:2</p>`) && c2.has(`<p id="v">v</p>`) })
	route.Store("y:3")
	h.Changed([]string{"tab:1"})
	waitFor(t, "the new route", func() bool { return c1.has(`<p id="nav">y:3</p>`) })
	h.mu.Lock()
	_, old := h.topics["x"]
	_, a := h.topics["src:A"]
	h.mu.Unlock()
	if old || !a {
		t.Fatalf("after the route moved: x kept %v, src:A kept %v", old, a)
	}
	extra := text("extra", []string{"deep:1:2:3"}, func(context.Context, *sql.Tx) (string, error) { return "e", nil })
	if !h.Watch("t2", extra) || !h.Unwatch("t2", "extra") {
		t.Fatal("Watch and Unwatch on t2")
	}
	h.mu.Lock()
	_, deep := h.topics["deep"]
	h.mu.Unlock()
	if deep {
		t.Fatal("deep kept after Unwatch")
	}
	c1.leave(t)
	c2.leave(t)
	h.mu.Lock()
	n := len(h.topics)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d topics left after the streams left", n)
	}
}

// A view's topics have no empty segment and no '*': Watch panics on one,
// and Depends fails its Render.
func TestTopicCheck(t *testing.T) {
	render := func(context.Context, *sql.Tx) (any, error) { return nil, nil }
	panics := func(f func()) (p bool) {
		defer func() { p = recover() != nil }()
		f()
		return false
	}
	h := New(Options{})
	ctx := context.WithValue(context.Background(), renderKey{}, &renderState{})
	for _, tp := range []string{"", ":", "a::b", ":a", "a:", "*", "a:*", "a:*:b", "a*", "*a:b"} {
		if !panics(func() { h.Watch("s", View{Key: "k", Topics: []string{"ok", tp}, Render: render}) }) {
			t.Errorf("Watch with the topic %q did not panic", tp)
		}
		if !panics(func() { Depends(ctx, "ok", tp) }) {
			t.Errorf("Depends(%q) did not panic", tp)
		}
	}
	for _, tp := range []string{"a", "board:42", "src:space:item", "a-b:c.d", Topic("x", "urn:uuid:1", "*", "")} {
		if panics(func() { h.Watch("s", View{Key: "k", Topics: []string{tp}, Render: render}) }) {
			t.Errorf("Watch with the topic %q panicked", tp)
		}
		if panics(func() { Depends(ctx, tp) }) {
			t.Errorf("Depends(%q) panicked", tp)
		}
	}
}

// Topic escapes what would make a level or a wildcard, keeps plain parts
// as they are, and its segments read back as the parts.
func TestTopic(t *testing.T) {
	for _, c := range []struct {
		parts []string
		want  string
	}{
		{[]string{"board", "42"}, "board:42"},
		{[]string{"src", "space", "item"}, "src:space:item"},
		{[]string{"cal", "urn:uuid:x"}, "cal:urn%3Auuid%3Ax"},
		{[]string{"a*"}, "a%2A"},
		{[]string{"*"}, "%2A"},
		{[]string{"x", "%3A"}, "x:%253A"},
		{[]string{"x", ""}, "x:%"},
		{[]string{"", ""}, "%:%"},
		{nil, ""},
	} {
		if got := Topic(c.parts...); got != c.want {
			t.Errorf("Topic(%q) = %q, want %q", c.parts, got, c.want)
		}
	}
	if Below(Topic("src", "a:b")) != "src:a%3Ab:*" {
		t.Fatalf("Below = %q", Below(Topic("src", "a:b")))
	}
	unescape := func(s string) string {
		if s == "%" {
			return ""
		}
		u, err := url.PathUnescape(s)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	r := rand.New(rand.NewPCG(5, 6))
	const alphabet = "ab:*%3A2"
	for range 10000 {
		parts := make([]string, 1+r.IntN(4))
		for i := range parts {
			b := make([]byte, r.IntN(5))
			for j := range b {
				b[j] = alphabet[r.IntN(len(alphabet))]
			}
			parts[i] = string(b)
		}
		tp := Topic(parts...)
		segs := strings.Split(tp, ":")
		if len(segs) != len(parts) || strings.Contains(tp, "*") {
			t.Fatalf("Topic(%q) = %q", parts, tp)
		}
		for i, s := range segs {
			if unescape(s) != parts[i] {
				t.Fatalf("Topic(%q) = %q: segment %d reads %q", parts, tp, i, unescape(s))
			}
		}
		checkTopic(tp)
	}
}
