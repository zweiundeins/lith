package live

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

// benchHub is a hub with n views of k topics each, every view watched by
// a stream of its own, and the render workers started without a read
// pool; Run is not called, the benchmark runs the ticks. The view of
// index i renders render(i).
func benchHub(b *testing.B, n, k int, render func(i int) func(context.Context, *sql.Tx) (any, error)) (*Hub, []*worker, []*stream) {
	h := New(Options{Workers: 4})
	ws := make([]*worker, h.workers)
	var wg sync.WaitGroup
	for i := range ws {
		ws[i] = &worker{h: h, start: make(chan *tick, 1), stmts: map[string]*stmt{}}
		wg.Go(ws[i].run)
	}
	b.Cleanup(func() {
		for _, w := range ws {
			close(w.start)
		}
		wg.Wait()
	})
	ss := make([]*stream, n)
	h.mu.Lock()
	for i := range ss {
		topics := make([]string, k)
		for j := range topics {
			topics[j] = "t:" + strconv.Itoa(i*k+j)
		}
		ss[i] = &stream{seq: uint64(i), wake: make(chan struct{}, 1)}
		h.watch(ss[i], View{Key: "v:" + strconv.Itoa(i), Topics: topics, Render: render(i)})
	}
	h.mu.Unlock()
	return h, ws, ss
}

// take empties the streams' pending frames, as their pumps would.
func take(ss []*stream) {
	for _, s := range ss {
		s.mu.Lock()
		for _, w := range s.watches {
			w.next = nil
		}
		s.mu.Unlock()
	}
}

// BenchmarkChanged is one commit's Changed on a hub of 1,000 views with
// 4 topics each: the commit touches two topics, which mark two views.
func BenchmarkChanged(b *testing.B) {
	h, _, _ := benchHub(b, 1000, 4, func(int) func(context.Context, *sql.Tx) (any, error) {
		return func(context.Context, *sql.Tx) (any, error) { return nil, nil }
	})
	topics := []string{"t:1", "t:2001"}
	for b.Loop() {
		h.Changed(topics)
		h.mu.Lock()
		for _, v := range h.dirty {
			v.dirty = false
		}
		clear(h.dirty)
		h.dirty = h.dirty[:0]
		h.mu.Unlock()
	}
}

// BenchmarkTick is one tick of n dirty views with 4 topics each, each
// watched by one stream, whose frame changes in every tick: the hub's own
// cost of a tick, with Renders that only format a number.
func BenchmarkTick(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			var gen int
			h, ws, ss := benchHub(b, n, 4, func(int) func(context.Context, *sql.Tx) (any, error) {
				return func(context.Context, *sql.Tx) (any, error) { return strconv.AppendInt(nil, int64(gen), 10), nil }
			})
			t := &tick{ctx: context.Background()}
			h.run(t, ws, time.Now())
			take(ss)
			for b.Loop() {
				gen++
				h.mu.Lock()
				for _, v := range h.views {
					h.mark(v)
				}
				h.mu.Unlock()
				h.run(t, ws, time.Now())
				take(ss)
			}
		})
	}
}
