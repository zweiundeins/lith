package writer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// A subscription ends with its ctx, and every one with the writer;
// Stats.Subscribers counts those open.
func TestSubscriptionEnds(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	subscribers := func(want int) {
		t.Helper()
		if n := f.w.Stats().Subscribers; n != want {
			t.Fatalf("%d subscribers, want %d", n, want)
		}
	}
	subscribers(0)
	ctx, cancel := context.WithCancel(context.Background())
	ended := f.w.Subscribe[any](ctx, 1)
	kept := f.w.Subscribe[any](context.Background(), 1)
	subscribers(2)
	publish := func() {
		t.Helper()
		if err := f.w.Do(context.Background(), Func(func(tx *Tx) error { tx.Publish(1); tx.Publish(2); return nil })); err != nil {
			t.Fatal(err)
		}
	}
	publish()
	cancel()
	closed(t, ended)
	subscribers(1)
	publish()
	// Each dropped the second of the first two; then kept dropped both,
	// and ended, gone, none.
	if st := f.w.Stats(); st.Dropped != 4 {
		t.Fatalf("dropped %d", st.Dropped)
	}
	done, stop := context.WithCancel(context.Background())
	stop()
	closed(t, f.w.Subscribe[any](done, 1))
	subscribers(1)

	f.w.Close()
	closed(t, kept)
	closed(t, f.w.Subscribe[any](context.Background(), 1))
	subscribers(0)
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

// A panic in an AfterCommit function or in OnCommit is logged, and the
// batch's values are delivered all the same: the batch has committed.
func TestPublishAfterPanics(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{OnCommit: func([]string) { panic("on commit") }})
	ch := f.w.Subscribe[int](t.Context(), 10)
	err := f.w.Do(context.Background(), Func(func(tx *Tx) error {
		tx.Publish(1)
		tx.AfterCommit(func() { panic("after commit") })
		tx.Publish(2)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := receive(t, ch, 2); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("got %v", got)
	}
	if log := f.log.String(); !strings.Contains(log, "after commit") || !strings.Contains(log, "on commit") {
		t.Fatalf("the panics were not logged:\n%s", log)
	}
}

// An Exclusive ends its batch: the values of the commands before it are
// delivered before it runs, those after it once their batch commits.
func TestPublishAroundExclusive(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ch := f.w.Subscribe[any](t.Context(), 10)
	ctx := context.Background()
	release := f.block(t)
	var wg sync.WaitGroup
	wg.Go(func() { f.w.Do(ctx, Func(func(tx *Tx) error { tx.Publish("before"); return nil })) })
	f.queued(t, 1)
	wg.Go(func() {
		f.w.Exclusive(ctx, func(context.Context, *sql.Conn) error {
			if len(ch) != 1 {
				t.Errorf("%d values delivered when Exclusive ran", len(ch))
			}
			return nil
		})
	})
	f.queued(t, 2)
	wg.Go(func() { f.w.Do(ctx, Func(func(tx *Tx) error { tx.Publish("after"); return nil })) })
	f.queued(t, 3)
	release()
	wg.Wait()
	if got := receive(t, ch, 2); !slices.Equal(got, []any{"before", "after"}) {
		t.Fatalf("got %v", got)
	}
}

// A command from Send publishes like one from Do.
func TestPublishFromSend(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ch := f.w.Subscribe[string](t.Context(), 10)
	if err := f.w.Send(Func(func(tx *Tx) error { tx.Publish("sent"); return nil })); err != nil {
		t.Fatal(err)
	}
	if err := f.w.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, ch, 1); got[0] != "sent" {
		t.Fatalf("got %v", got)
	}
}

// A subscription opened after a commit and before its delivery, here from
// OnCommit, gets the batch's values.
func TestSubscribeBeforeDelivery(t *testing.T) {
	got := make(chan (<-chan int), 1)
	var w *Writer
	f := start(t, sqlite.Options{}, Options{OnCommit: func([]string) {
		select {
		case got <- w.Subscribe[int](context.Background(), 1):
		default:
		}
	}})
	w = f.w
	if err := f.w.Do(context.Background(), Func(func(tx *Tx) error { tx.Publish(7); return nil })); err != nil {
		t.Fatal(err)
	}
	if v := receive(t, <-got, 1); v[0] != 7 {
		t.Fatalf("got %v", v)
	}
}

type card struct{}

// Delivery matches the value's dynamic type: a *card reaches no
// Subscribe[card], and counts as published but not as dropped.
func TestPublishMatchesTheDynamicType(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	values := f.w.Subscribe[card](t.Context(), 1)
	pointers := f.w.Subscribe[*card](t.Context(), 1)
	if err := f.w.Do(context.Background(), Func(func(tx *Tx) error { tx.Publish(&card{}); return nil })); err != nil {
		t.Fatal(err)
	}
	receive(t, pointers, 1)
	empty(t, values)
	if st := f.w.Stats(); st.Published != 1 || st.Dropped != 0 {
		t.Fatalf("published %d, dropped %d", st.Published, st.Dropped)
	}
}

// afterFuncs is a context with an AfterFunc method, which context.AfterFunc
// calls: it counts the registrations, and those neither run nor stopped,
// and calls hook, if set, on each registration and stop. Its Done is its
// own and its Value nil, so context.AfterFunc does not take it for a
// cancelCtx.
type afterFuncs struct {
	done        chan struct{}
	end         sync.Once
	calls, open atomic.Int64
	hook        func()
	ending      bool // the context ends while AfterFunc registers
}

func newAfterFuncs() *afterFuncs { return &afterFuncs{done: make(chan struct{})} }

func (c *afterFuncs) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *afterFuncs) Done() <-chan struct{}       { return c.done }
func (c *afterFuncs) Value(any) any               { return nil }
func (c *afterFuncs) cancel()                     { c.end.Do(func() { close(c.done) }) }

func (c *afterFuncs) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

func (c *afterFuncs) AfterFunc(f func()) func() bool {
	c.calls.Add(1)
	c.open.Add(1)
	if c.hook != nil {
		c.hook()
	}
	var once sync.Once
	settle := func() (first bool) {
		once.Do(func() { first = true; c.open.Add(-1) })
		return first
	}
	stopped := make(chan struct{})
	go func() {
		select {
		case <-c.done:
			if settle() {
				f()
			}
		case <-stopped:
		}
	}()
	if c.ending {
		c.cancel()
	}
	return func() bool {
		if c.hook != nil {
			c.hook()
		}
		if !settle() {
			return false
		}
		close(stopped)
		return true
	}
}

// Subscribe and Close run the context's AfterFunc and its stop outside the
// bus's lock, so a context whose code reads the writer does not deadlock.
func TestSubscribeCallsTheContextOutsideTheLock(t *testing.T) {
	db := openDB(t, sqlite.Options{})
	w, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	go w.Run(context.Background())
	// A deadlocked bus holds Close: give up on it rather than wait for go
	// test's timeout.
	t.Cleanup(func() {
		closed := make(chan struct{})
		go func() {
			w.Close()
			db.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
		}
	})
	ctx := newAfterFuncs()
	ctx.hook = func() { w.Stats() }
	done := make(chan struct{})
	go func() {
		w.Subscribe[any](ctx, 1)
		w.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe or Close deadlocked in the context's AfterFunc")
	}
	if ctx.calls.Load() != 1 || ctx.open.Load() != 0 {
		t.Fatalf("%d AfterFunc calls, %d left", ctx.calls.Load(), ctx.open.Load())
	}
}

// A subscription whose ctx ends, and whose AfterFunc ran and found nothing
// to remove, before Subscribe added it ends all the same. context.AfterFunc
// runs f in a goroutine of its own, which leaves that order to chance, so
// the bus gets an AfterFunc that runs f at once.
func TestSubscribeWhoseContextEndsBeforeItJoins(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	f.w.bus.afterFunc = func(_ context.Context, fn func()) func() bool {
		cancel()
		fn()
		return func() bool { return false }
	}
	closed(t, f.w.Subscribe[any](ctx, 1))
	if n := f.w.Stats().Subscribers; n != 0 {
		t.Fatalf("%d subscribers", n)
	}
}

// A subscription whose ctx ends while Subscribe registers its wait ends
// all the same.
func TestSubscribeWhoseContextEndsAsItOpens(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := newAfterFuncs()
	ctx.ending = true
	closed(t, f.w.Subscribe[any](ctx, 1))
	if n := f.w.Stats().Subscribers; n != 0 || ctx.calls.Load() != 1 {
		t.Fatalf("%d subscribers, %d AfterFunc calls", n, ctx.calls.Load())
	}
}

// Subscriptions whose ctx ends while they open end all the same.
func TestSubscribeRacesItsContext(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	for range 200 {
		ctx := newAfterFuncs()
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() { f.w.Subscribe[any](ctx, 1) })
		}
		ctx.cancel()
		wg.Wait()
		waitFor(t, "the subscriptions to end with their ctx", func() bool {
			return f.w.Stats().Subscribers == 0 && ctx.open.Load() == 0
		})
	}
}

// A Subscribe that races Close leaves no wait for its ctx behind: the
// subscription is open and Close ends it, or it never opens.
func TestSubscribeRacesClose(t *testing.T) {
	for range 20 {
		f := start(t, sqlite.Options{}, Options{})
		ctx := newAfterFuncs()
		var wg, running sync.WaitGroup
		var closed atomic.Bool
		for range 8 {
			running.Add(1)
			wg.Go(func() {
				running.Done()
				for !closed.Load() {
					f.w.Subscribe[any](ctx, 1)
				}
			})
		}
		running.Wait()
		time.Sleep(time.Millisecond)
		f.w.Close()
		closed.Store(true)
		wg.Wait()
		if ctx.calls.Load() == 0 {
			t.Fatal("context.AfterFunc never called the context's AfterFunc")
		}
		if n := ctx.open.Load(); n != 0 {
			t.Fatalf("%d waits for ctx left after Close", n)
		}
		if n := f.w.Stats().Subscribers; n != 0 {
			t.Fatalf("%d subscribers after Close", n)
		}
	}
}
