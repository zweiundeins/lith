package writer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sqlite"
)

// receive takes n values from ch, or fails the test after 5 s.
func receive[T any](t *testing.T, ch <-chan T, n int) []T {
	t.Helper()
	var got []T
	timeout := time.After(5 * time.Second)
	for len(got) < n {
		select {
		case v, ok := <-ch:
			if !ok {
				t.Fatalf("closed after %v", got)
			}
			got = append(got, v)
		case <-timeout:
			t.Fatalf("got %v, want %d values", got, n)
		}
	}
	return got
}

// empty fails the test if ch holds a value.
func empty[T any](t *testing.T, ch <-chan T) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("got %v", v)
	default:
	}
}

// closed fails the test unless ch is closed within 5 s.
func closed[T any](t *testing.T, ch <-chan T) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("the channel stayed open")
		}
	}
}

// The values of the commands that commit arrive in commit order, after
// AfterCommit and OnCommit and before Do returns; a failed command's, a
// rolled-back savepoint's and a failed batch's never do.
func TestPublishOrder(t *testing.T) {
	// ch stays empty until the first batch with values delivers them, so
	// that batch's AfterCommit and OnCommit must find it empty.
	var ch <-chan any
	var watch atomic.Bool
	var early atomic.Int32
	notYet := func() {
		if watch.Load() && len(ch) > 0 {
			early.Add(1)
		}
	}
	f := start(t, sqlite.Options{}, Options{OnCommit: func([]string) { notYet() }})
	ch = f.w.Subscribe[any](t.Context(), 100)
	ctx := context.Background()

	release := f.block(t)
	watch.Store(true)
	cmds := []Command{
		Func(func(tx *Tx) error {
			tx.Publish(1)
			tx.AfterCommit(notYet)
			tx.Publish(2)
			return nil
		}),
		Func(func(tx *Tx) error { tx.Publish("failed"); return errors.New("no") }),
		Func(func(tx *Tx) error {
			tx.Publish(3)
			tx.Savepoint(func(tx *Tx) error { tx.Publish("inner"); return errors.New("no") })
			tx.Savepoint(func(tx *Tx) error { tx.Publish(4); return nil })
			tx.Untouched(func(tx *Tx) error { tx.Publish(5); return nil })
			tx.Publish(nil)
			return nil
		}),
		Func(func(tx *Tx) error { tx.Publish("panicked"); panic("broken") }),
		Once("op", Func(func(tx *Tx) error { tx.Publish(6); return nil })),
	}
	var wg sync.WaitGroup
	for i, cmd := range cmds {
		wg.Go(func() {
			f.w.Do(ctx, cmd)
			if i == 0 && len(ch) == 0 {
				t.Error("Do returned before the values were delivered")
			}
		})
		f.queued(t, i+1)
	}
	release()
	wg.Wait()
	watch.Store(false)
	if n := early.Load(); n > 0 {
		t.Fatalf("AfterCommit or OnCommit found the values delivered %d times", n)
	}
	if err := f.w.Do(ctx, Once("op", Func(func(tx *Tx) error { tx.Publish("replayed"); return nil }))); err != nil {
		t.Fatal(err)
	}
	if err := f.w.Do(ctx, Func(func(tx *Tx) error { tx.Publish("alone"); return errors.New("no") })); err == nil {
		t.Fatal("no error")
	}
	// A command that ends the transaction fails its whole batch.
	release = f.block(t)
	f.w.Send(Func(func(tx *Tx) error { tx.Publish("batch"); return nil }))
	f.w.Send(Func(func(tx *Tx) error { _, err := tx.Exec("ROLLBACK"); return err }))
	f.queued(t, 2)
	release()
	if err := f.w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.w.Do(ctx, Func(func(tx *Tx) error { tx.Publish(7); return nil })); err != nil {
		t.Fatal(err)
	}

	got := receive(t, ch, 7)
	if want := []any{1, 2, 3, 4, 5, 6, 7}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	empty(t, ch)
	if st := f.w.Stats(); st.Published != 7 || st.Dropped != 0 {
		t.Fatalf("published %d, dropped %d", st.Published, st.Dropped)
	}
}

// OnceResult drops the values of a refused command, as it undoes its
// changes, and keeps those of the command that records the refusal.
func TestPublishOnceResultRefusal(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ch := f.w.Subscribe[string](t.Context(), 10)
	err := f.w.Do(context.Background(), Func(func(tx *Tx) error {
		tx.Publish("outer")
		var refusal *Refusal
		err := OnceResult("op", ResultFunc(func(tx *Tx) ([]byte, error) {
			tx.Publish("refused")
			return nil, &Refusal{Msg: "full"}
		})).Apply(tx)
		if !errors.As(err, &refusal) {
			return err
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := receive(t, ch, 1); got[0] != "outer" {
		t.Fatalf("got %v", got)
	}
	empty(t, ch)
}

type stamp int

func (s stamp) String() string { return fmt.Sprint("stamp ", int(s)) }

// A subscriber gets the values of its type; a full queue drops values for
// its subscriber alone, counted, and never holds up the writer.
func TestPublishTypesAndDrops(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := t.Context()
	all := f.w.Subscribe[any](ctx, 100)
	stamps := f.w.Subscribe[stamp](ctx, 100)
	stringers := f.w.Subscribe[fmt.Stringer](ctx, 100)
	full := f.w.Subscribe[stamp](ctx, 2) // never read
	others := f.w.Subscribe[float64](ctx, 1)
	for i := range 10 {
		err := f.w.Do(context.Background(), Func(func(tx *Tx) error {
			tx.Publish(stamp(i))
			tx.Publish(i)
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := receive(t, all, 20); got[0] != stamp(0) || got[1] != 0 || got[19] != 9 {
		t.Fatalf("all got %v", got)
	}
	if got := receive(t, stamps, 10); !slices.Equal(got, []stamp{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}) {
		t.Fatalf("stamps got %v", got)
	}
	if got := receive(t, stringers, 10); got[9].String() != "stamp 9" {
		t.Fatalf("stringers got %v", got)
	}
	if got := receive(t, full, 2); !slices.Equal(got, []stamp{0, 1}) {
		t.Fatalf("the full queue kept %v", got)
	}
	empty(t, full)
	empty(t, others)
	if st := f.w.Stats(); st.Published != 20 || st.Dropped != 8 {
		t.Fatalf("published %d, dropped %d", st.Published, st.Dropped)
	}
}

// A subscription ends with its ctx, and every one with the writer.
func TestSubscriptionEnds(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	ended := f.w.Subscribe[any](ctx, 1)
	kept := f.w.Subscribe[any](context.Background(), 1)
	publish := func() {
		t.Helper()
		if err := f.w.Do(context.Background(), Func(func(tx *Tx) error { tx.Publish(1); tx.Publish(2); return nil })); err != nil {
			t.Fatal(err)
		}
	}
	publish()
	cancel()
	closed(t, ended)
	publish()
	// Each dropped the second of the first two; then kept dropped both,
	// and ended, gone, none.
	if st := f.w.Stats(); st.Dropped != 4 {
		t.Fatalf("dropped %d", st.Dropped)
	}
	done, stop := context.WithCancel(context.Background())
	stop()
	closed(t, f.w.Subscribe[any](done, 1))

	f.w.Close()
	closed(t, kept)
	closed(t, f.w.Subscribe[any](context.Background(), 1))
}

// Subscriptions come and go from other goroutines while the writer
// publishes.
func TestSubscribeWhilePublishing(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	stop := make(chan struct{})
	var publisher, subscribers sync.WaitGroup
	publisher.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			f.w.Do(context.Background(), Func(func(tx *Tx) error { tx.Publish(1); return nil }))
		}
	})
	for range 8 {
		subscribers.Go(func() {
			for range 50 {
				ctx, cancel := context.WithCancel(context.Background())
				ch := f.w.Subscribe[int](ctx, 4)
				for range 2 {
					select {
					case <-ch:
					case <-time.After(5 * time.Second):
						t.Error("nothing delivered")
					}
				}
				cancel()
				for range ch {
				}
			}
		})
	}
	subscribers.Wait()
	close(stop)
	publisher.Wait()
}
