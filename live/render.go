package live

import "context"

// renderState is what the Renders of a worker's tick find in their ctx.
type renderState struct {
	seq uint64 // Hub.Seq before the tick's read transaction began
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
