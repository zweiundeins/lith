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
	// The workers count each view as they render it, and a view whose
	// stream has not taken its last frame yet renders in a later tick: wait
	// for all three, and for a:b:c:d's frame, whose tick then has published,
	// so a:x would have rendered in it too.
	waitFor(t, "a, a:b and a:b:c:d", func() bool {
		r := h.Stats().RendersByKey
		return r["a"] >= 3 && r["a:b"] >= 3 && r["a:b:c:d"] >= 2 && strings.Count(string(c.bytes()), `<p id="a:b:c:d">`) == 2
	})
	r := h.Stats().RendersByKey
	if r["a"] != 3 || r["a:b"] != 3 || r["a:b:c:d"] != 2 || r["a:x"] != 1 {
		t.Fatalf("renders %v", r)
	}
	c.leave(t)
}

// One commit that names two topics under a:b counts a:b and a once.
func TestTopicTreeDedupes(t *testing.T) {
	h := treeHub(Options{}, "a", "a:b")
	h.Changed([]string{"x"})
	h.Changed([]string{"a:b:1", "a:b:2", "a:b"})
	if h.Seq() != 2 {
		t.Fatalf("seq %d after two commits", h.Seq())
	}
	for _, tp := range []string{"a", "a:b", "a:b:1", "a:b:2"} {
		if v := h.Version(tp); v != 2 {
			t.Errorf("Version(%s) = %d, want 2", tp, v)
		}
	}
	h.mu.Lock()
	n := len(h.dirty)
	h.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d views on the dirty list, want 2", n)
	}
	if h.Version("x") != 1 || h.Version("a:c") != 0 {
		t.Fatalf("Version(x) = %d, Version(a:c) = %d", h.Version("x"), h.Version("a:c"))
	}
}

// Against a model of random commits, :* and MarkAll among them: a topic
// reads at least the last commit that touched it, never less, and exactly
// that while both stores keep everything. A topic above another reads at
// least as much as it.
func TestTopicTreeVersions(t *testing.T) {
	for _, c := range []struct {
		name           string
		leaves, prefix int
		exact          bool
	}{
		{"kept", 0, 0, true},
		{"pushed out", 8, 4, false},
		{"none", -1, -1, false},
		{"no prefixes", 0, -1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := New(Options{Versions: c.leaves, PrefixVersions: c.prefix})
			r := rand.New(rand.NewPCG(3, 4))
			own, below := map[string]uint64{}, map[string]uint64{}
			var floor uint64
			var all []string
			name := func() string {
				parts := make([]string, 1+r.IntN(4))
				for i := range parts {
					parts[i] = string(rune('a' + r.IntN(3)))
				}
				return strings.Join(parts, ":")
			}
			above := func(tp string, f func(string)) {
				for i := strings.LastIndexByte(tp, ':'); i >= 0; i = strings.LastIndexByte(tp[:i], ':') {
					f(tp[:i])
				}
			}
			want := func(tp string) uint64 {
				n := max(floor, own[tp], below[tp])
				above(tp, func(a string) { n = max(n, below[a]) })
				return n
			}
			for range 5000 {
				topics := make([]string, 1+r.IntN(3))
				for i := range topics {
					topics[i] = name()
					if r.IntN(10) == 0 {
						topics[i] += ":*"
					}
				}
				if r.IntN(500) == 0 {
					topics = append(topics, "*")
				}
				h.Changed(topics)
				n := h.Seq()
				for _, tp := range topics {
					if tp == "*" {
						floor = n
						continue
					}
					tp, star := strings.CutSuffix(tp, ":*")
					all = append(all, tp)
					own[tp] = n
					if star {
						below[tp] = n
					}
					above(tp, func(a string) { own[a] = n })
				}
				if k := len(h.leaves.newer) + len(h.leaves.older); c.leaves > 0 && k > 2*c.leaves || c.leaves < 0 && k > 0 {
					t.Fatalf("%d versions kept, Versions %d", k, c.leaves)
				}
				if k := len(h.prefixes.newer) + len(h.prefixes.older); c.prefix > 0 && k > 2*c.prefix || c.prefix < 0 && k > 0 {
					t.Fatalf("%d prefixes kept, PrefixVersions %d", k, c.prefix)
				}
				for range 4 {
					tp := all[r.IntN(len(all))]
					got := h.Version(tp)
					if w := want(tp); got < w || c.exact && got != w {
						t.Fatalf("after commit %d: Version(%s) = %d, want %d", n, tp, got, w)
					}
					if got := h.Version(tp + ":*"); got != h.Version(tp) {
						t.Fatalf("Version(%s:*) = %d, Version(%s) = %d", tp, got, tp, h.Version(tp))
					}
					above(tp, func(a string) {
						if h.Version(a) < got {
							t.Fatalf("after commit %d: Version(%s) = %d, below it Version(%s) = %d", n, a, h.Version(a), tp, got)
						}
					})
				}
			}
			if h.Version("*") != h.Seq() {
				t.Fatalf("Version(*) = %d, seq %d", h.Version("*"), h.Seq())
			}
		})
	}
}

// A sync that touches ten times Options.Versions items under src:A pushes
// out items, and leaves src:B's version as it was: prefixes have a store
// of their own.
func TestTopicTreeSync(t *testing.T) {
	const keep = 64
	h := New(Options{Versions: keep})
	h.Changed([]string{"src:B:1"})
	h.Changed([]string{"src:B:2", "other"})
	b, item := h.Version("src:B"), h.Version("src:B:1")
	if b != 2 || item != 1 {
		t.Fatalf("Version(src:B) = %d, Version(src:B:1) = %d", b, item)
	}
	for i := range 10 * keep {
		h.Changed([]string{"src:A:" + strconv.Itoa(i), "src:A:" + strconv.Itoa(i) + ":body"})
	}
	if v := h.Version("src:B"); v != b {
		t.Fatalf("after the sync, Version(src:B) = %d, want %d", v, b)
	}
	if v, n := h.Version("src:A"), h.Seq(); v != n || h.Version("src") != n {
		t.Fatalf("after the sync, Version(src:A) = %d, Version(src) = %d, seq %d", v, h.Version("src"), n)
	}
	if v := h.Version("src:B:1"); v <= item {
		t.Fatalf("Version(src:B:1) = %d, pushed out, want above %d", v, item)
	}
	if v := h.Version("other"); v <= 2 {
		t.Fatalf("Version(other) = %d, pushed out, want above 2", v)
	}
	// A topic named directly that the hub keeps as a prefix stays there.
	h.Changed([]string{"src:B"})
	b = h.Seq()
	for i := range 10 * keep {
		h.Changed([]string{"src:A:" + strconv.Itoa(i)})
	}
	if v := h.Version("src:B"); v != b {
		t.Fatalf("named, then a sync: Version(src:B) = %d, want %d", v, b)
	}
}

// A view that declares a coarse topic with Depends renders for commits
// below it, and for a :* above it, and not for one on a sibling. A
// :* that lands after a Render's snapshot, above a topic the Render
// declares for the first time, renders the view again.
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

	h2 := treeHub(Options{})
	w := &view{key: "w"}
	seq := h2.Seq()
	h2.Changed([]string{"src:*"})
	h2.mu.Lock()
	stale := h2.redepend(w, []string{"src:A:7"}, seq)
	h2.redepend(w, nil, seq)
	h2.mu.Unlock()
	if !stale {
		t.Fatal("a :* after the snapshot, above a new topic, did not render the view again")
	}
}

// The tree marks what a map from each view to its topics would, while
// views come and go on topics that a few views share and on topics that
// many share, and it ends empty, its spare nodes and maps clean.
func TestTopicTreeModel(t *testing.T) {
	h := New(Options{})
	r := rand.New(rand.NewPCG(5, 6))
	views := make([]*view, 12)
	subs := make([]map[string]bool, len(views))
	for i := range views {
		views[i] = &view{key: strconv.Itoa(i)}
		subs[i] = map[string]bool{}
	}
	name := func() string {
		parts := make([]string, 1+r.IntN(3))
		for i := range parts {
			parts[i] = string(rune('a' + r.IntN(3)))
		}
		return strings.Join(parts, ":")
	}
	under := func(tp, c string) bool { return tp == c || strings.HasPrefix(tp, c+":") }
	check := func() {
		t.Helper()
		for tp, n := range h.topics {
			if n.name != tp || n.empty() || n.up != nil && n.up != h.topics[tp[:strings.LastIndexByte(tp, ':')]] {
				t.Fatalf("node %q: name %q, empty %v", tp, n.name, n.empty())
			}
			if n.up != nil {
				if _, ok := n.up.kids[n]; !ok {
					t.Fatalf("node %q is not among its parent's", tp)
				}
			}
			if n.many != nil && len(n.few) > 0 {
				t.Fatalf("node %q has views in a slice and a map", tp)
			}
		}
		for _, m := range h.spareKids {
			if len(m) > 0 {
				t.Fatal("a spare map holds nodes")
			}
		}
	}
	for step := range 20000 {
		i := r.IntN(len(views))
		tp := name()
		h.mu.Lock()
		if subs[i][tp] {
			h.unsubscribe(views[i], tp)
			delete(subs[i], tp)
		} else {
			h.subscribe(views[i], tp)
			subs[i][tp] = true
		}
		h.mu.Unlock()
		if step%7 != 0 {
			continue
		}
		c := name()
		star := r.IntN(4) == 0
		commit := c
		if star {
			commit += ":*"
		}
		h.Changed([]string{commit})
		var want []string
		for k, ts := range subs {
			for tp := range ts {
				if under(c, tp) || star && under(tp, c) {
					want = append(want, views[k].key)
					break
				}
			}
		}
		slices.Sort(want)
		if got := dirtyKeys(h); !slices.Equal(got, want) {
			t.Fatalf("step %d, commit %s: marked %v, want %v", step, commit, got, want)
		}
		check()
	}
	h.mu.Lock()
	for k, ts := range subs {
		for tp := range ts {
			h.unsubscribe(views[k], tp)
		}
	}
	h.mu.Unlock()
	check()
	if len(h.topics) != 0 || len(h.spare) == 0 {
		t.Fatalf("%d nodes left, %d spare", len(h.topics), len(h.spare))
	}
}

// A view that listens to a topic twice listens to it once, as a map would
// have it: one unsubscribe ends it.
func TestTopicTreeOnce(t *testing.T) {
	h := New(Options{})
	v := &view{key: "v"}
	h.mu.Lock()
	h.subscribe(v, "a:b")
	h.subscribe(v, "a:b")
	h.unsubscribe(v, "a:b")
	n := len(h.topics)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d nodes after one unsubscribe", n)
	}
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
