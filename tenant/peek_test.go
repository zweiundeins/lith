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

// Peek returns an open value pinned, and reports false for a key that is
// not open, without opening it.
func TestPeekNeverOpens(t *testing.T) {
	tr := newTracker()
	p := New(4, tr.open)
	defer closePool(t, p, tr)

	if v, release, ok, err := p.Peek("a"); ok || err != nil || release != nil || v != nil {
		t.Fatalf("Peek of a key never opened = %v, %v, %v", v, ok, err)
	}
	if st := p.Stats(); st.Open != 0 || st.Opens != 0 {
		t.Fatalf("Peek opened the key: %+v", st)
	}
	a, release := mustGet(t, p, "a")
	release()
	v, release, ok, err := p.Peek("a")
	if !ok || err != nil || v != a {
		t.Fatalf("Peek of an open key = %v, %v, %v; want the value Get opened", v, ok, err)
	}
	if st := p.Stats(); st.Pinned != 1 || st.Opens != 1 {
		t.Fatalf("Stats while Peek pins = %+v", st)
	}
	release()
	release()
	if st := p.Stats(); st.Pinned != 0 {
		t.Fatalf("Stats after the release = %+v", st)
	}
	if err := p.Evict(bg, "a"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := p.Peek("a"); ok || err != nil {
		t.Fatalf("Peek of an evicted key = %v, %v", ok, err)
	}
	if tr.opened != 1 {
		t.Fatalf("%d opens", tr.opened)
	}
}

// A value Peek pins is not evicted, and releasing it makes it the most
// recently released, as with Get.
func TestPeekPinsLikeGet(t *testing.T) {
	tr := newTracker()
	p := New(2, tr.open)
	defer closePool(t, p, tr)

	_, release := mustGet(t, p, "a")
	release()
	_, release = mustGet(t, p, "b")
	release()
	// a is the least recently released until Peek's release.
	_, release, ok, err := p.Peek("a")
	if !ok || err != nil {
		t.Fatalf("Peek(a) = %v, %v", ok, err)
	}
	release()
	_, release = mustGet(t, p, "c")
	release()
	if _, _, ok, _ := p.Peek("b"); ok {
		t.Error("b is open: the cold open of c evicted a, whose last release was Peek's")
	}
	a, release, ok, err := p.Peek("a")
	if !ok || err != nil {
		t.Fatalf("Peek(a) after the eviction = %v, %v", ok, err)
	}
	// Pinned by Peek, a stays open past the cap.
	for _, key := range []string{"d", "e"} {
		_, r := mustGet(t, p, key)
		r()
	}
	if a.closed.Load() {
		t.Fatal("a value Peek pinned was evicted")
	}
	release()
}

// blocking is a value whose Close waits until it is told to go on.
type blocking struct {
	closing chan struct{}
	finish  chan struct{}
}

func (b *blocking) Close() error {
	close(b.closing)
	<-b.finish
	return nil
}

// Peek does not wait for an open or a close of the key in flight: the key
// is not open meanwhile.
func TestPeekDoesNotWait(t *testing.T) {
	tr := newTracker()
	opening, started := tr.gate("a")
	p := New(4, tr.open)
	defer closePool(t, p, tr)
	got := make(chan error, 1)
	go func() {
		_, release, err := p.Get(bg, "a")
		if err == nil {
			release()
		}
		got <- err
	}()
	<-started
	if _, _, ok, err := p.Peek("a"); ok || err != nil {
		t.Fatalf("Peek of a key being opened = %v, %v", ok, err)
	}
	opening()
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	if _, release, ok, err := p.Peek("a"); !ok || err != nil {
		t.Fatalf("Peek once opened = %v, %v", ok, err)
	} else {
		release()
	}

	b := &blocking{closing: make(chan struct{}), finish: make(chan struct{})}
	q := New(4, func(context.Context, string) (*blocking, error) { return b, nil })
	_, release, err := q.Get(bg, "b")
	if err != nil {
		t.Fatal(err)
	}
	release()
	evicted := make(chan error, 1)
	go func() { evicted <- q.Evict(bg, "b") }()
	<-b.closing
	if _, _, ok, err := q.Peek("b"); ok || err != nil {
		t.Fatalf("Peek of a key being closed = %v, %v", ok, err)
	}
	close(b.finish)
	if err := <-evicted; err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
}

// A denied key returns the reason and no value, also while its value stays
// open, until Allow; after Close every key returns ErrClosed.
func TestPeekRefusesADeniedKey(t *testing.T) {
	tr := newTracker()
	p := New(4, tr.open)
	_, release := mustGet(t, p, "a")
	release()
	restoring := errors.New("restoring")
	p.Deny("a", restoring)
	if v, release, ok, err := p.Peek("a"); ok || !errors.Is(err, restoring) || release != nil || v != nil {
		t.Fatalf("Peek of a denied open key = %v, %v, %v", v, ok, err)
	}
	if st := p.Stats(); st.Open != 1 {
		t.Fatalf("Stats = %+v, want a still open", st)
	}
	p.Deny("b", nil)
	if _, _, ok, err := p.Peek("b"); ok || !errors.Is(err, ErrDenied) {
		t.Fatalf("Peek of a key denied without a reason = %v, %v", ok, err)
	}
	p.Allow("a")
	if _, release, ok, err := p.Peek("a"); !ok || err != nil {
		t.Fatalf("Peek after Allow = %v, %v", ok, err)
	} else {
		release()
	}
	closePool(t, p, tr)
	if _, _, ok, err := p.Peek("a"); ok || !errors.Is(err, ErrClosed) {
		t.Fatalf("Peek after Close = %v, %v", ok, err)
	}
}

type viaGet struct{}

// Peek beside Get, Evict, Deny and the evictions of cold opens: it never
// opens a key, returns the value of its key or nothing, a value it pinned
// is not closed but by Evict, a key denied for good stays refused, and a
// key is still never open twice.
func TestPeekConcurrentWithGetEvictAndDeny(t *testing.T) {
	tr := newTracker()
	tr.delay = func(string) time.Duration { return time.Duration(rand.IntN(1000)) * time.Microsecond }
	p := New(3, func(ctx context.Context, key string) (*value, error) {
		if ctx.Value(viaGet{}) == nil {
			t.Errorf("%s opened, but not by Get", key)
		}
		return tr.open(ctx, key)
	})
	ctx := context.WithValue(bg, viaGet{}, true)
	gone, erased := errors.New("gone"), errors.New("erased")
	p.Deny("x", erased)
	// e keys are evicted and denied, s keys only leave by the cap.
	key := func() string {
		if rand.IntN(2) == 0 {
			return fmt.Sprintf("e%d", rand.IntN(4))
		}
		return fmt.Sprintf("s%d", rand.IntN(6))
	}

	var hits atomic.Int64
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			for range 200 {
				k := key()
				_, release, err := p.Get(ctx, k)
				if err != nil {
					if !errors.Is(err, gone) {
						t.Error(err)
					}
					continue
				}
				time.Sleep(time.Duration(rand.IntN(200)) * time.Microsecond)
				release()
			}
		})
	}
	// The peeks go on until the Gets and evictions are done.
	done := make(chan struct{})
	var peeks sync.WaitGroup
	for range 6 {
		peeks.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				k := key()
				v, release, ok, err := p.Peek(k)
				switch {
				case err != nil:
					if !errors.Is(err, gone) {
						t.Errorf("Peek(%s): %v", k, err)
					}
					continue
				case !ok:
					if v != nil || release != nil {
						t.Errorf("Peek(%s) returned a value and a release with ok false", k)
					}
					time.Sleep(time.Duration(rand.IntN(50)) * time.Microsecond)
					continue
				}
				hits.Add(1)
				if v.key != k {
					t.Errorf("Peek(%s) returned the value for %s", k, v.key)
				}
				time.Sleep(time.Duration(rand.IntN(200)) * time.Microsecond)
				if k[0] == 's' && v.closed.Load() {
					t.Errorf("%s was closed while Peek pinned it", k)
				}
				release()
				if _, _, ok, err := p.Peek("x"); ok || !errors.Is(err, erased) {
					t.Errorf("Peek of a key denied for good = %v, %v", ok, err)
				}
			}
		})
	}
	wg.Go(func() {
		for range 150 {
			k := fmt.Sprintf("e%d", rand.IntN(4))
			if rand.IntN(3) == 0 {
				p.Deny(k, gone)
			}
			if err := p.Evict(bg, k); err != nil {
				t.Error(err)
			}
			time.Sleep(time.Duration(rand.IntN(500)) * time.Microsecond)
			p.Allow(k)
		}
	})
	wg.Wait()
	close(done)
	peeks.Wait()
	if hits.Load() == 0 {
		t.Error("Peek never found a key open")
	}
	if st := p.Stats(); st.Pinned != 0 {
		t.Errorf("Stats = %+v, want no pins left", st)
	}
	closePool(t, p, tr)
}
