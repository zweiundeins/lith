package tenant

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tracker counts the values a test opener hands out, so tests can check that
// a key is never open twice and that every value gets closed exactly once.
type tracker struct {
	mu     sync.Mutex
	live   map[string]int
	most   int
	opened int
	closed int
	delay  func(key string) time.Duration
	fail   func(key string) error
}

func newTracker() *tracker { return &tracker{live: map[string]int{}} }

type value struct {
	key    string
	t      *tracker
	closed atomic.Bool
}

func (v *value) Close() error {
	if v.closed.Swap(true) {
		return errors.New("closed twice")
	}
	v.t.mu.Lock()
	defer v.t.mu.Unlock()
	v.t.live[v.key]--
	v.t.closed++
	return nil
}

func (t *tracker) open(ctx context.Context, key string) (*value, error) {
	if t.delay != nil {
		time.Sleep(t.delay(key))
	}
	if t.fail != nil {
		if err := t.fail(key); err != nil {
			return nil, err
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.live[key]++
	t.most = max(t.most, t.live[key])
	t.opened++
	return &value{key: key, t: t}, nil
}

// check fails the test unless no key was ever open twice and, once the pool
// is closed, every value was closed.
func (t *tracker) check(tb testing.TB) {
	tb.Helper()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.most > 1 {
		tb.Errorf("a key was open %d times at once", t.most)
	}
	if t.opened != t.closed {
		tb.Errorf("opened %d values, closed %d", t.opened, t.closed)
	}
}

// gate makes the tracker's opens of key block until the returned func is
// called; started is closed once the first such open is waiting.
func (t *tracker) gate(key string) (open func(), started <-chan struct{}) {
	g := make(chan struct{})
	s := make(chan struct{})
	var once sync.Once
	prev := t.delay
	t.delay = func(k string) time.Duration {
		if k == key {
			once.Do(func() { close(s) })
			<-g
		}
		if prev != nil {
			return prev(k)
		}
		return 0
	}
	return func() { close(g) }, s
}

var bg = context.Background()

func mustGet(t *testing.T, p *Pool[*value], key string) (*value, func()) {
	t.Helper()
	v, release, err := p.Get(bg, key)
	if err != nil {
		t.Fatalf("Get(%s): %v", key, err)
	}
	return v, release
}

func closePool(t *testing.T, p *Pool[*value], tr *tracker) {
	t.Helper()
	if err := p.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	tr.check(t)
}

func TestGetCachesTheValue(t *testing.T) {
	tr := newTracker()
	p := New(4, tr.open)
	defer closePool(t, p, tr)

	a, release := mustGet(t, p, "a")
	release()
	b, release := mustGet(t, p, "a")
	release()
	if a != b {
		t.Error("a second Get opened a new value")
	}
	if st := p.Stats(); st.Open != 1 || st.Opens != 1 {
		t.Errorf("Stats = %+v, want one open value", st)
	}
}

func TestEvictsTheLeastRecentlyReleased(t *testing.T) {
	tr := newTracker()
	p := New(2, tr.open)
	defer closePool(t, p, tr)

	a, releaseA := mustGet(t, p, "a")
	b, releaseB := mustGet(t, p, "b")
	releaseA()
	releaseB()
	_, releaseC := mustGet(t, p, "c")
	defer releaseC()

	waitFor(t, func() bool { return a.closed.Load() })
	if b.closed.Load() {
		t.Error("b was closed, but a had been idle longer")
	}
	if st := p.Stats(); st.Open != 2 || st.Evictions != 1 {
		t.Errorf("Stats = %+v, want 2 open after 1 eviction", st)
	}
}

func TestNeverEvictsAPinnedValue(t *testing.T) {
	tr := newTracker()
	p := New(2, tr.open)
	defer closePool(t, p, tr)

	var held []*value
	for _, k := range []string{"a", "b", "c", "d"} {
		v, release := mustGet(t, p, k)
		defer release()
		held = append(held, v)
	}
	for _, v := range held {
		if v.closed.Load() {
			t.Errorf("%s was closed while pinned", v.key)
		}
	}
	if st := p.Stats(); st.Open != 4 || st.Pinned != 4 {
		t.Errorf("Stats = %+v, want the soft cap exceeded: Open 4, Pinned 4", st)
	}
}

func TestAColdOpenTrimsBackToTheCap(t *testing.T) {
	tr := newTracker()
	p := New(2, tr.open)
	defer closePool(t, p, tr)

	var releases []func()
	for _, k := range []string{"a", "b", "c", "d"} {
		_, release := mustGet(t, p, k)
		releases = append(releases, release)
	}
	for _, release := range releases {
		release()
	}
	_, release := mustGet(t, p, "e")
	release()
	if st := p.Stats(); st.Open != 2 || st.Evictions != 3 {
		t.Errorf("Stats = %+v, want Open 2 after 3 evictions", st)
	}
}

func TestASlowOpenHoldsUpOnlyItsKey(t *testing.T) {
	tr := newTracker()
	p := New(4, tr.open)
	defer closePool(t, p, tr)
	open, started := tr.gate("slow")
	defer open()

	go func() {
		if _, release, err := p.Get(bg, "slow"); err == nil {
			release()
		}
	}()
	<-started

	done := make(chan error, 1)
	go func() {
		_, release, err := p.Get(bg, "fast")
		if err == nil {
			release()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Get(fast) waited for another key's open")
	}
}

func TestConcurrentColdGetsShareOneOpen(t *testing.T) {
	tr := newTracker()
	p := New(4, tr.open)
	defer closePool(t, p, tr)
	open, started := tr.gate("a")

	const n = 8
	got := make(chan *value, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			v, release, err := p.Get(bg, "a")
			if err != nil {
				t.Error(err)
				return
			}
			got <- v
			release()
		})
	}
	<-started
	time.Sleep(20 * time.Millisecond)
	open()
	wg.Wait()
	close(got)

	first := <-got
	for v := range got {
		if v != first {
			t.Error("Gets returned different values for one key")
		}
	}
	if st := p.Stats(); st.Opens != 1 {
		t.Errorf("Opens = %d, want 1", st.Opens)
	}
}

func TestGetGivesUpWaitingWithItsContext(t *testing.T) {
	tr := newTracker()
	p := New(4, tr.open)
	defer closePool(t, p, tr)
	open, started := tr.gate("a")

	opened := make(chan struct{})
	go func() {
		defer close(opened)
		if _, release, err := p.Get(bg, "a"); err == nil {
			release()
		}
	}()
	<-started

	ctx, cancel := context.WithTimeout(bg, 20*time.Millisecond)
	defer cancel()
	if _, _, err := p.Get(ctx, "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Get while another open is in flight = %v, want DeadlineExceeded", err)
	}
	open()
	<-opened
	if st := p.Stats(); st.Open != 1 {
		t.Errorf("Open = %d, want the first open to have finished", st.Open)
	}
}

func TestTheOpenerContextIsNotCancelled(t *testing.T) {
	var cancelled atomic.Bool
	p := New(4, func(ctx context.Context, key string) (*value, error) {
		time.Sleep(30 * time.Millisecond)
		cancelled.Store(ctx.Err() != nil)
		return &value{key: key, t: newTracker()}, nil
	})
	defer p.Close()

	ctx, cancel := context.WithTimeout(bg, 5*time.Millisecond)
	defer cancel()
	if _, release, err := p.Get(ctx, "a"); err == nil {
		release()
	}
	if cancelled.Load() {
		t.Error("the opener saw the caller's cancellation")
	}
}

func TestAFailedOpenIsNotCached(t *testing.T) {
	tr := newTracker()
	boom := errors.New("boom")
	var fails atomic.Int32
	fails.Store(1)
	tr.fail = func(string) error {
		if fails.Add(-1) >= 0 {
			return boom
		}
		return nil
	}
	p := New(4, tr.open)
	defer closePool(t, p, tr)

	if _, _, err := p.Get(bg, "a"); !errors.Is(err, boom) {
		t.Fatalf("Get = %v, want the opener's error", err)
	}
	_, release := mustGet(t, p, "a")
	release()
}

func TestWaitersShareAFailedOpen(t *testing.T) {
	tr := newTracker()
	down := errors.New("replica unreachable")
	var calls atomic.Int32
	tr.fail = func(string) error {
		if calls.Add(1) == 1 {
			return down
		}
		return nil
	}
	p := New(4, tr.open)
	defer closePool(t, p, tr)
	open, started := tr.gate("a")

	const n = 5
	errs := make(chan error, n)
	go func() {
		_, _, err := p.Get(bg, "a")
		errs <- err
	}()
	<-started
	for range n - 1 {
		go func() {
			_, _, err := p.Get(bg, "a")
			errs <- err
		}()
	}
	time.Sleep(20 * time.Millisecond)
	open()
	for range n {
		if err := <-errs; !errors.Is(err, down) {
			t.Errorf("Get = %v, want the failed open's error", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("opener ran %d times, want once for all waiters", got)
	}
	_, release := mustGet(t, p, "a")
	release()
}

func TestAPanickingCloseInEvictFreesTheKey(t *testing.T) {
	var panicked atomic.Bool
	p := New(4, func(ctx context.Context, key string) (panicCloser, error) {
		return panicCloser{&panicked}, nil
	})
	defer p.Close()
	_, release, err := p.Get(bg, "a")
	if err != nil {
		t.Fatal(err)
	}
	release()

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the value's panic did not reach Evict's caller")
			}
		}()
		_ = p.Evict(bg, "a")
	}()

	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	_, release, err = p.Get(ctx, "a")
	if err != nil {
		t.Fatalf("Get after a panicking close: %v", err)
	}
	release()
}

// panicCloser panics on its first Close in a test.
type panicCloser struct{ done *atomic.Bool }

func (c panicCloser) Close() error {
	if !c.done.Swap(true) {
		panic("close bug")
	}
	return nil
}

func TestAPanickingOpenerFreesItsKey(t *testing.T) {
	tr := newTracker()
	var panics atomic.Bool
	panics.Store(true)
	tr.fail = func(string) error {
		if panics.Swap(false) {
			panic("opener bug")
		}
		return nil
	}
	p := New(4, tr.open)
	defer closePool(t, p, tr)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the opener's panic did not reach the caller")
			}
		}()
		_, _, _ = p.Get(bg, "a")
	}()

	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	_, release, err := p.Get(ctx, "a")
	if err != nil {
		t.Fatalf("Get after a panicking open: %v", err)
	}
	release()
}

func TestDenyRefusesTheKey(t *testing.T) {
	tr := newTracker()
	p := New(4, tr.open)
	defer closePool(t, p, tr)
	restoring := errors.New("restoring")

	p.Deny("a", restoring)
	if _, _, err := p.Get(bg, "a"); !errors.Is(err, restoring) {
		t.Errorf("Get on a denied key = %v, want its reason", err)
	}
	p.Deny("b", nil)
	if _, _, err := p.Get(bg, "b"); !errors.Is(err, ErrDenied) {
		t.Errorf("Get on a key denied without a reason = %v, want ErrDenied", err)
	}
	_, release := mustGet(t, p, "c")
	release()

	p.Allow("a")
	_, release = mustGet(t, p, "a")
	release()
}

func TestAKeyDeniedWhileOpeningIsDiscarded(t *testing.T) {
	tr := newTracker()
	p := New(4, tr.open)
	defer closePool(t, p, tr)
	open, started := tr.gate("a")
	erased := errors.New("erased")

	got := make(chan error, 1)
	go func() {
		_, release, err := p.Get(bg, "a")
		if err == nil {
			release()
		}
		got <- err
	}()
	<-started
	p.Deny("a", erased)

	evicted := make(chan error, 1)
	go func() { evicted <- p.Evict(bg, "a") }()
	open()

	if err := <-got; !errors.Is(err, erased) {
		t.Errorf("Get for a key denied mid-open = %v, want the deny reason", err)
	}
	if err := <-evicted; err != nil {
		t.Errorf("Evict: %v", err)
	}
	if st := p.Stats(); st.Open != 0 {
		t.Errorf("Open = %d, want 0", st.Open)
	}
}

func TestEvictClosesAPinnedValueAndItsStalePinIsHarmless(t *testing.T) {
	tr := newTracker()
	p := New(1, tr.open)
	defer closePool(t, p, tr)

	old, releaseOld := mustGet(t, p, "a")
	if err := p.Evict(bg, "a"); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if !old.closed.Load() {
		t.Fatal("Evict left the value open")
	}

	fresh, releaseFresh := mustGet(t, p, "a")
	defer releaseFresh()
	if fresh == old {
		t.Fatal("Get after Evict returned the closed value")
	}
	releaseOld() // must not unpin the fresh value
	_, releaseB := mustGet(t, p, "b")
	defer releaseB()
	if fresh.closed.Load() {
		t.Error("a stale release unpinned the next value, which was then evicted")
	}
}

func TestEvictOfAKeyNotOpenIsANoOp(t *testing.T) {
	tr := newTracker()
	p := New(1, tr.open)
	defer closePool(t, p, tr)
	if err := p.Evict(bg, "never"); err != nil {
		t.Errorf("Evict = %v, want nil", err)
	}
}

func TestReleaseTwiceCountsOnce(t *testing.T) {
	tr := newTracker()
	p := New(4, tr.open)
	defer closePool(t, p, tr)

	_, r1 := mustGet(t, p, "a")
	_, r2 := mustGet(t, p, "a")
	r1()
	r1()
	if st := p.Stats(); st.Pinned != 1 {
		t.Errorf("Pinned = %d after releasing one of two pins twice, want 1", st.Pinned)
	}
	r2()
}

func TestDoPinsForTheCall(t *testing.T) {
	tr := newTracker()
	p := New(4, tr.open)
	defer closePool(t, p, tr)

	err := p.Do(bg, "a", func(v *value) error {
		if st := p.Stats(); st.Pinned != 1 {
			t.Errorf("Pinned = %d inside Do, want 1", st.Pinned)
		}
		return errors.New("from fn")
	})
	if err == nil || err.Error() != "from fn" {
		t.Errorf("Do = %v, want fn's error", err)
	}
	if st := p.Stats(); st.Pinned != 0 || st.Open != 1 {
		t.Errorf("Stats after Do = %+v, want the value open and unpinned", st)
	}
}

func TestCloseWaitsForAnOpenAndClosesEverything(t *testing.T) {
	tr := newTracker()
	p := New(4, tr.open)
	open, started := tr.gate("slow")

	_, releaseA := mustGet(t, p, "a")
	defer releaseA()
	got := make(chan error, 1)
	go func() {
		_, _, err := p.Get(bg, "slow")
		got <- err
	}()
	<-started

	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	time.Sleep(20 * time.Millisecond)
	open()

	if err := <-closed; err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := <-got; !errors.Is(err, ErrClosed) {
		t.Errorf("Get finishing after Close = %v, want ErrClosed", err)
	}
	if _, _, err := p.Get(bg, "a"); !errors.Is(err, ErrClosed) {
		t.Errorf("Get after Close = %v, want ErrClosed", err)
	}
	tr.check(t)
}

func TestCloseReportsCloseErrors(t *testing.T) {
	p := New(4, func(ctx context.Context, key string) (failingCloser, error) { return failingCloser{}, nil })
	_, release, err := p.Get(bg, "a")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := p.Close(); !errors.Is(err, errClose) {
		t.Errorf("Close = %v, want the value's close error", err)
	}
}

var errClose = errors.New("close failed")

type failingCloser struct{}

func (failingCloser) Close() error { return errClose }

// TestConcurrentUseWithEviction runs many goroutines over more keys than the
// pool holds, with a slow opener: a key is never open twice, a pinned value
// is never closed, and nothing leaks. There are more keys than goroutines, so
// cold opens keep finding idle values to evict.
func TestConcurrentUseWithEviction(t *testing.T) {
	tr := newTracker()
	tr.delay = func(string) time.Duration { return time.Duration(rand.IntN(2000)) * time.Microsecond }
	p := New(4, tr.open)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 150 {
				key := fmt.Sprintf("k%d", rand.IntN(40))
				v, release, err := p.Get(bg, key)
				if err != nil {
					t.Error(err)
					return
				}
				if v.key != key {
					t.Errorf("Get(%s) returned the value for %s", key, v.key)
				}
				time.Sleep(time.Duration(rand.IntN(300)) * time.Microsecond)
				if v.closed.Load() {
					t.Errorf("%s was closed while pinned", key)
				}
				release()
			}
		})
	}
	wg.Wait()
	if st := p.Stats(); st.Evictions == 0 || st.Pinned != 0 {
		t.Errorf("Stats = %+v, want evictions and no pins left", st)
	}
	closePool(t, p, tr)
}

// TestConcurrentUseWithEvictAndDeny adds explicit evictions and denials to
// the mix; they may close values in use, but a key is still never open twice.
func TestConcurrentUseWithEvictAndDeny(t *testing.T) {
	tr := newTracker()
	tr.delay = func(string) time.Duration { return time.Duration(rand.IntN(1000)) * time.Microsecond }
	p := New(3, tr.open)
	gone := errors.New("gone")

	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			for range 150 {
				key := fmt.Sprintf("k%d", rand.IntN(8))
				err := p.Do(bg, key, func(v *value) error {
					time.Sleep(time.Duration(rand.IntN(200)) * time.Microsecond)
					return nil
				})
				if err != nil && !errors.Is(err, gone) {
					t.Error(err)
				}
			}
		})
	}
	for range 2 {
		wg.Go(func() {
			for range 100 {
				key := fmt.Sprintf("k%d", rand.IntN(8))
				if rand.IntN(4) == 0 {
					p.Deny(key, gone)
				}
				if err := p.Evict(bg, key); err != nil {
					t.Error(err)
				}
				time.Sleep(time.Duration(rand.IntN(500)) * time.Microsecond)
				p.Allow(key)
			}
		})
	}
	wg.Wait()
	closePool(t, p, tr)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(time.Millisecond)
	}
}
