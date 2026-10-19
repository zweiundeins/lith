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
		plain = plain && p != "" && strings.IndexAny(p, ":*%") < 0
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
