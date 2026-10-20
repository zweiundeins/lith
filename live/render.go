package live

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
)

// renderState is what the Renders of a worker's tick find in their ctx.
type renderState struct {
	seq  uint64       // Hub.Seq before the tick's read transaction began
	size atomic.Int64 // the length of the last []byte frame of the view under way, -1 for none

	mu   sync.Mutex
	deps []string // declared by the Render under way
}

type renderKey struct{}

func stateFrom(ctx context.Context) *renderState {
	s, _ := ctx.Value(renderKey{}).(*renderState)
	return s
}

// Seq is, in a Render, the number of the last commit its snapshot is sure
// to hold (Hub.Seq): the hub reads it before the read transaction begins,
// so a later commit may be in the snapshot as well. A value the Render
// computes from its snapshot is good as of that number: keep it while
// Hub.Version of the topics it reads is not above it.
//
//	if c, ok := counts.Get(space); ok && hub.Version("space:"+space) <= c.seq {
//		return c.n
//	}
//	n := countItems(ctx, q, space)
//	counts.Add(space, count{n: n, seq: live.Seq(ctx)})
//
// Outside a Render it is 0, which keeps a value good only while none of
// its topics has changed since the hub started. A GET reads Hub.Seq
// before its read transaction begins instead.
func Seq(ctx context.Context) uint64 {
	if s := stateFrom(ctx); s != nil {
		return s.seq
	}
	return 0
}

// FrameBuffer is, in a Render, an empty buffer for the view's frame, with
// room for its last []byte frame and an eighth more: a Render that formats
// its frame into it (sse.AppendPatchElements, sse.NewElements) allocates
// the frame once while it keeps about its size. It is nil before the
// view's first []byte frame, and outside a Render.
func FrameBuffer(ctx context.Context) []byte {
	s := stateFrom(ctx)
	if s == nil {
		return nil
	}
	n := s.size.Load()
	if n <= 0 {
		return nil
	}
	return make([]byte, 0, n+n/8)
}

// Depends declares, in a Render, that the view read what the topics name,
// as Topics do: a commit that touches one of them marks the view dirty.
// The topics the Render declares replace those the Render before it
// declared, once it returns, so a view whose reads depend on a row, such
// as a tab's route, declares what it read for the route it rendered, and
// a commit to what an earlier route read no longer renders it. A Render
// that fails keeps the topics of the one before. A commit that lands
// after the Render's snapshot and touches a topic the view takes on marks
// it dirty as well: the hub compares the topic's Version with Seq(ctx).
// A topic that View.Topics would refuse panics, which fails the Render.
// Outside a Render, Depends does nothing.
func Depends(ctx context.Context, topics ...string) {
	if s := stateFrom(ctx); s != nil {
		for _, t := range topics {
			checkTopic(t)
		}
		s.mu.Lock()
		s.deps = append(s.deps, topics...)
		s.mu.Unlock()
	}
}

// declared is what a Render declared with Depends.
type declared struct {
	topics  []string // sorted, without duplicates
	seq     uint64   // Seq(ctx) of the Render
	changed bool     // the topics differ from the view's
}

// take returns what the Render of v that just returned declared, against
// the view's topics, and makes room for the next Render's. Topics that
// differ go into v's spare slice, which redepend swaps with v.deps, so
// that a view whose topics move costs no new slice.
func (s *renderState) take(v *view, failed bool) declared {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.deps
	if len(d) == 0 && len(v.deps) == 0 {
		return declared{}
	}
	s.deps = s.deps[:0]
	if failed {
		return declared{}
	}
	slices.Sort(d)
	d = slices.Compact(d)
	if slices.Equal(d, v.deps) {
		return declared{}
	}
	v.spare = append(v.spare[:0], d...)
	return declared{topics: v.spare, seq: s.seq, changed: true}
}
