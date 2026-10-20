package live

import (
	"fmt"
	"strings"
)

// node is a topic in the hub's tree: one that a view listens to, or one
// above such a topic, as board is above board:42. A node goes once no
// view listens to it or below it. Guarded by Hub.mu.
type node struct {
	name  string
	up    *node
	views map[*view]struct{}
	kids  map[*node]struct{}
	// The last commit that marked the views of the node and of those
	// above it, and the last that marked those of every node below it.
	marked, swept uint64
}

// node returns t's node, and creates it and those above it that are
// missing. Hub.mu is held.
func (h *Hub) node(t string) *node {
	if n := h.topics[t]; n != nil {
		return n
	}
	n := &node{name: t}
	h.topics[t] = n
	if i := strings.LastIndexByte(t, ':'); i >= 0 {
		up := h.node(t[:i])
		if up.kids == nil {
			up.kids = map[*node]struct{}{}
		}
		up.kids[n] = struct{}{}
		n.up = up
	}
	return n
}

// subscribe has a commit that touches t mark v dirty. Hub.mu is held.
func (h *Hub) subscribe(v *view, t string) {
	n := h.node(t)
	if n.views == nil {
		n.views = map[*view]struct{}{}
	}
	n.views[v] = struct{}{}
}

// unsubscribe undoes subscribe, and drops the nodes it leaves empty.
// Hub.mu is held.
func (h *Hub) unsubscribe(v *view, t string) {
	n := h.topics[t]
	if n == nil {
		return
	}
	delete(n.views, v)
	for ; n != nil && len(n.views) == 0 && len(n.kids) == 0; n = n.up {
		delete(h.topics, n.name)
		if n.up != nil {
			delete(n.up.kids, n)
		}
	}
}

// markUp marks the views of t and of the topics above it for commit seq,
// and reports whether it marked one that was not dirty. Hub.mu is held.
func (h *Hub) markUp(t string, seq uint64) bool {
	n := h.topics[t]
	for n == nil {
		i := strings.LastIndexByte(t, ':')
		if i < 0 {
			return false
		}
		t = t[:i]
		n = h.topics[t]
	}
	return h.markFrom(n, seq)
}

// markFrom marks the views of n and of the nodes above it. Hub.mu is
// held.
func (h *Hub) markFrom(n *node, seq uint64) bool {
	marked := false
	for ; n != nil && n.marked != seq; n = n.up {
		n.marked = seq
		for v := range n.views {
			marked = h.mark(v) || marked
		}
	}
	return marked
}

// markBelow marks the views of t, of every topic below it and of those
// above it. Hub.mu is held.
func (h *Hub) markBelow(t string, seq uint64) bool {
	n := h.topics[t]
	if n == nil {
		return h.markUp(t, seq)
	}
	marked := h.sweep(n, seq)
	return h.markFrom(n.up, seq) || marked
}

// sweep marks the views of n and of every node below it. Hub.mu is held.
func (h *Hub) sweep(n *node, seq uint64) bool {
	if n.swept == seq {
		return false
	}
	n.marked, n.swept = seq, seq
	marked := false
	for v := range n.views {
		marked = h.mark(v) || marked
	}
	for k := range n.kids {
		marked = h.sweep(k, seq) || marked
	}
	return marked
}

// versions keeps the versions of the topics changed last, in two
// generations: a topic in neither changed at floor or before.
type versions[V any] struct {
	newer, older map[string]V
	floor        uint64
	rotated      uint64 // the commit at which older stopped taking topics
	keep         int
}

func newVersions[V any](keep int) versions[V] {
	return versions[V]{newer: map[string]V{}, older: map[string]V{}, keep: keep}
}

func (s *versions[V]) get(t string) (V, bool) {
	if v, ok := s.newer[t]; ok {
		return v, true
	}
	v, ok := s.older[t]
	return v, ok
}

// room makes room in the newer generation for a topic it does not hold,
// which commit n touched: once it is full, it starts a new one, and the
// older goes. It reports false when the store keeps no topics.
func (s *versions[V]) room(n uint64) bool {
	if s.keep < 0 {
		s.floor = n
		return false
	}
	if len(s.newer) >= s.keep {
		// Every topic of the older generation changed at s.rotated or before.
		s.floor, s.rotated = s.rotated, n
		s.older, s.newer = s.newer, s.older
		clear(s.newer)
	}
	return true
}

// forget drops every topic, as changed at n.
func (s *versions[V]) forget(n uint64) {
	clear(s.newer)
	clear(s.older)
	s.floor, s.rotated = n, n
}

// stamp is what the hub keeps of a prefix: the last commit that touched
// it, and the last that touched everything below it (a topic with :*).
type stamp struct{ own, below uint64 }

// setVersion records that commit n named t. A topic the hub keeps as a
// prefix stays with the prefixes, so that its version is in one store,
// the one that holds it. Hub.vmu is held.
func (h *Hub) setVersion(t string, n uint64) {
	if p := h.prefixes.newer[t]; p != nil {
		p.own = n
		return
	}
	if h.prefixes.older[t] != nil {
		h.setPrefix(t, n, false)
		return
	}
	if _, ok := h.leaves.newer[t]; ok || h.leaves.room(n) {
		h.leaves.newer[t] = n
	}
}

// setPrefix records that commit n touched t, and with below everything
// below it. It reports false when n had touched t already. Hub.vmu is
// held.
func (h *Hub) setPrefix(t string, n uint64, below bool) bool {
	s := &h.prefixes
	p := s.newer[t]
	if p == nil {
		if !s.room(n) {
			return true
		}
		if p = s.older[t]; p == nil {
			p = &stamp{s.floor, s.floor}
		}
		s.newer[t] = p
	}
	if p.own == n && (!below || p.below == n) {
		return false
	}
	p.own = n
	if below {
		p.below = n
	}
	return true
}

// touch records that commit n touched t and the topics above it, and
// with :* everything below t. Hub.vmu is held.
func (h *Hub) touch(t string, n uint64) {
	if p, ok := strings.CutSuffix(t, ":*"); ok {
		t = p
		h.setPrefix(t, n, true)
	} else {
		h.setVersion(t, n)
	}
	for i := strings.LastIndexByte(t, ':'); i >= 0; i = strings.LastIndexByte(t[:i], ':') {
		// The topics above one that n touched already have n.
		if !h.setPrefix(t[:i], n, false) {
			break
		}
	}
}

// version is t's version: the last commit that touched t, or everything
// below it or below a topic above it. Hub.vmu is held.
func (h *Hub) version(t string) uint64 {
	var n uint64
	if p, ok := h.prefixes.get(t); ok {
		n = max(p.own, p.below)
	} else {
		// t may have been a prefix the hub no longer keeps.
		n = h.prefixes.floor
		if v, ok := h.leaves.get(t); ok {
			n = max(n, v)
		} else {
			n = max(n, h.leaves.floor)
		}
	}
	for i := strings.LastIndexByte(t, ':'); i >= 0; i = strings.LastIndexByte(t[:i], ':') {
		if p, ok := h.prefixes.get(t[:i]); ok {
			n = max(n, p.below)
		} else {
			n = max(n, h.prefixes.floor)
		}
	}
	return n
}

// checkTopic panics on a topic a view cannot listen to.
func checkTopic(t string) {
	ok := t != "" && t[0] != ':' && t[len(t)-1] != ':'
	for i := 0; ok && i < len(t); i++ {
		ok = t[i] != '*' && (t[i] != ':' || t[i+1] != ':')
	}
	if !ok {
		panic(fmt.Sprintf("live: topic %q: a view's topic is segments joined by ':', none empty, without '*'", t))
	}
}

// Topic joins parts into a topic, one segment each: Topic("src", space,
// uid). It escapes ':', '*' and '%' in a part as %3A, %2A and %25, so an
// id from outside the app, such as urn:uuid:..., stays one segment and
// never reads as a level or as everything below. A part without them
// comes out as it is, so Topic("board", "42") is board:42. An empty part
// becomes %, which no other part escapes to.
func Topic(parts ...string) string {
	n := len(parts)
	plain := true
	for _, p := range parts {
		n += len(p)
		plain = plain && p != "" && !strings.ContainsAny(p, ":*%")
	}
	if plain {
		return strings.Join(parts, ":")
	}
	var b strings.Builder
	b.Grow(n + 8)
	for i, p := range parts {
		if i > 0 {
			b.WriteByte(':')
		}
		if p == "" {
			b.WriteByte('%')
			continue
		}
		for j := 0; j < len(p); j++ {
			switch p[j] {
			case ':':
				b.WriteString("%3A")
			case '*':
				b.WriteString("%2A")
			case '%':
				b.WriteString("%25")
			default:
				b.WriteByte(p[j])
			}
		}
	}
	return b.String()
}

// Below is the topic a commit touches to say that everything at or
// below t changed: t with :*, for Changed and writer.Tx.Touch.
func Below(t string) string { return t + ":*" }
