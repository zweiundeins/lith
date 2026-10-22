package writer

import (
	"context"
	"slices"
	"sync"
)

// Subscribe returns a channel of the values that committed commands
// publish (Tx.Publish) and whose dynamic type is T, or implements T when T
// is an interface: Subscribe[any] gets all of them. A value published as
// *Card reaches no Subscribe[Card]; Stats.Published counts it all the
// same, and Stats.Dropped does not.
//
// The values arrive in commit order: batch by batch, the commands of a
// batch in their order, and a command's values in the order it published
// them. The writer hands them over on its goroutine once the batch's
// AfterCommit functions and OnCommit have run, before any Do of the batch
// returns: by then each value is in its subscribers' channels, not yet
// handled. The writer never waits: the channel holds n values (at least
// 1), and a value that finds it full is dropped for this subscriber alone
// and counted in Stats.Dropped. The channel closes once ctx ends or the
// writer stops.
//
// Every subscriber gets the same value, so publish values that nobody
// changes afterwards: a pointer, a map or a slice is shared.
//
// Delivery is best effort and in process. A value is lost when its
// subscriber's queue is full, and when the process stops between the
// commit and the delivery. A subscription gets the values the writer
// delivers while it is open, including one whose Tx.Publish ran before
// Subscribe was called, and none delivered before; nothing is replayed. A
// listener that must not miss anything reconciles from the data now and
// then, or reads the events package's streams instead.
//
//	go func() {
//		for m := range w.Subscribe[CardMoved](ctx, 256) {
//			notify(m.Board, m.Card)
//		}
//	}()
func (w *Writer) Subscribe[T any](ctx context.Context, n int) <-chan T {
	s := &subscription[T]{ch: make(chan T, max(n, 1))}
	if !w.bus.add(ctx, s) {
		close(s.ch)
	}
	return s.ch
}

// A subscriber takes the values of the types it wants.
type subscriber interface {
	// offer hands v over unless the queue is full, if v is of the
	// subscriber's type.
	offer(v any) (wanted, taken bool)
	close()
}

type subscription[T any] struct{ ch chan T }

func (s *subscription[T]) offer(v any) (wanted, taken bool) {
	t, ok := v.(T)
	if !ok {
		return false, false
	}
	select {
	case s.ch <- t:
		return true, true
	default:
		return true, false
	}
}

func (s *subscription[T]) close() { close(s.ch) }

// bus holds the subscriptions. The writer delivers under mu, so a channel
// never closes while the writer sends on it.
type bus struct {
	mu     sync.Mutex
	subs   []entry
	closed bool // the writer stopped
}

type entry struct {
	s    subscriber
	stop func() bool // ends the wait for the subscription's ctx
}

func (e entry) end() {
	e.stop()
	e.s.close()
}

// add opens s until ctx ends, unless ctx has ended or the writer stopped.
func (b *bus) add(ctx context.Context, s subscriber) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || ctx.Err() != nil {
		return false
	}
	stop := context.AfterFunc(ctx, func() { b.remove(s) })
	b.subs = append(b.subs, entry{s: s, stop: stop})
	return true
}

func (b *bus) remove(s subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, e := range b.subs {
		if e.s == s {
			b.subs = slices.Delete(b.subs, i, i+1)
			e.end()
			return
		}
	}
}

// close ends every subscription once the writer has stopped.
func (b *bus) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, e := range b.subs {
		e.end()
	}
	clear(b.subs)
	b.subs = nil
}

func (b *bus) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// deliver hands the values of a committed batch to the subscribers.
func (w *Writer) deliver(values []any) {
	w.published.Add(int64(len(values)))
	b := &w.bus
	b.mu.Lock()
	defer b.mu.Unlock()
	var dropped int64
	for _, v := range values {
		for _, e := range b.subs {
			if wanted, taken := e.s.offer(v); wanted && !taken {
				dropped++
			}
		}
	}
	if dropped > 0 {
		w.dropped.Add(dropped)
	}
}
