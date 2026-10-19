package live

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
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

// BenchmarkTickDepends is BenchmarkTick with 1,000 views that declare 4
// topics each with Depends besides their 4 Topics: the same in every tick
// (same), or other ones in every tick, as if every tab navigated (moved).
func BenchmarkTickDepends(b *testing.B) {
	for _, moved := range []bool{false, true} {
		b.Run(map[bool]string{false: "same", true: "moved"}[moved], func(b *testing.B) {
			var gen int
			h, ws, ss := benchHub(b, 1000, 4, func(i int) func(context.Context, *sql.Tx) (any, error) {
				var deps [2][]string
				for k := range deps {
					for j := range 4 {
						deps[k] = append(deps[k], "d:"+strconv.Itoa(k)+":"+strconv.Itoa(i*4+j))
					}
				}
				return func(ctx context.Context, _ *sql.Tx) (any, error) {
					k := 0
					if moved {
						k = gen % 2
					}
					Depends(ctx, deps[k]...)
					return strconv.AppendInt(nil, int64(gen), 10), nil
				}
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

// TestVersionMemory measures what the versions of the topics changed last
// hold on the heap, the topics' names of 16 bytes included, with both
// generations full:
//
//	go test ./live -run TestVersionMemory -v
func TestVersionMemory(t *testing.T) {
	const keep = 1 << 15
	heap := func() uint64 {
		var m runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	before := heap()
	h := New(Options{})
	topics := make([]string, 1)
	for i := range 2*keep - 1 {
		topics[0] = fmt.Sprintf("item:%011d", i)
		h.Changed(topics)
	}
	after := heap()
	n := len(h.leaves.newer) + len(h.leaves.older)
	per := float64(after-before) / float64(n)
	t.Logf("%d versions hold %d KiB, %.0f bytes each", n, (after-before)>>10, per)
	if n != 2*keep-1 || per > 128 {
		t.Fatalf("%d versions, %.0f bytes each", n, per)
	}
	runtime.KeepAlive(h)
}
