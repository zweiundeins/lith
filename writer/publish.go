package writer

import (
	"context"
	"slices"
	"sync"
)

// Subscribe returns a channel of the values that committed commands
// publish (Tx.Publish) and that are of type T: Subscribe[any] gets all of
// them, an interface type the values that implement it. They arrive in
// commit order: batch by batch, the commands of a batch in their order,
// and a command's values in the order it published them. The writer
// hands them over on its goroutine once the batch's AfterCommit functions
// and OnCommit have run, before any Do of the batch returns, and never
// waits: the channel holds n values (at least 1), and a value that finds
// it full is dropped for this subscriber alone and counted in
// Stats.Dropped. The channel closes once ctx ends or the writer stops.
//
// The bus is best effort and in process. A value is lost when its
// subscriber's queue is full, and when the process stops between the
// commit and the delivery; a subscriber gets nothing published before it
// subscribed, and nothing is replayed. A listener that must not miss
// anything reconciles from the data now and then, or reads the events
// package's streams instead.
//
//	go func() {
//		for m := range w.Subscribe[CardMoved](ctx, 256) {
//			notify(m.Board, m.Card)
//		}
//	}()
func (w *Writer) Subscribe[T any](ctx context.Context, n int) <-chan T {
	s := &subscription[T]{ch: make(chan T, max(n, 1))}
	if ctx.Err() != nil || !w.bus.add(s) {
		close(s.ch)
		return s.ch
	}
	stop := context.AfterFunc(ctx, func() { w.bus.remove(s) })
	w.bus.mu.Lock()
	s.stop = stop
	w.bus.mu.Unlock()
	return s.ch
}

// A subscriber takes the values of the types it wants.
type subscriber interface {
	// offer hands v over unless the queue is full, if v is of the
	// subscriber's type.
	offer(v any) (wanted, taken bool)
	end()
}

type subscription[T any] struct {
	ch   chan T
	stop func() bool // ends the wait for ctx
}

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

func (s *subscription[T]) end() {
	if s.stop != nil {
		s.stop()
	}
	close(s.ch)
}

// bus holds the subscriptions. The writer delivers under mu, so a channel
// never closes while the writer sends on it.
type bus struct {
	mu     sync.Mutex
	subs   []subscriber
	closed bool // the writer stopped
}

func (b *bus) add(s subscriber) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.subs = append(b.subs, s)
	return true
}

func (b *bus) remove(s subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, x := range b.subs {
		if x == s {
			b.subs = slices.Delete(b.subs, i, i+1)
			s.end()
			return
		}
	}
}

// close ends every subscription once the writer has stopped.
func (b *bus) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, s := range b.subs {
		s.end()
	}
	clear(b.subs)
	b.subs = nil
}

// deliver hands the values of a committed batch to the subscribers.
func (w *Writer) deliver(values []any) {
	w.published.Add(int64(len(values)))
	b := &w.bus
	b.mu.Lock()
	defer b.mu.Unlock()
	var dropped int64
	for _, v := range values {
		for _, s := range b.subs {
			if wanted, taken := s.offer(v); wanted && !taken {
				dropped++
			}
		}
	}
	if dropped > 0 {
		w.dropped.Add(dropped)
	}
}
