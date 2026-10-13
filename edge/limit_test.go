package edge

import (
	"math/rand/v2"
	"strconv"
	"sync"
	"testing"
	"time"
)

// t0 has no monotonic reading, so the tests drive the wall clock.
var t0 = time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)

func TestLimiterBucket(t *testing.T) {
	l := NewLimiter(20, 60)
	if !l.Allow("a", 60, t0) || l.Allow("a", 1, t0) {
		t.Fatal("a full bucket gives its burst, then nothing")
	}
	if !l.Allow("b", 10, t0) {
		t.Error("keys share a bucket")
	}
	if !l.Allow("a", 10, t0.Add(500*time.Millisecond)) || l.Allow("a", 1, t0.Add(500*time.Millisecond)) {
		t.Error("half a second at 20/s should give back exactly 10 tokens")
	}
	if !l.Allow("a", 60, t0.Add(time.Hour)) || l.Allow("a", 61, t0.Add(2*time.Hour)) {
		t.Error("a bucket fills to its burst and no further")
	}
	if !l.Allow("a", 0, t0.Add(2*time.Hour)) {
		t.Error("taking nothing is always allowed")
	}
}

func TestLimiterSurvivesAClockStep(t *testing.T) {
	l := NewLimiter(1, 5)
	for range 5 {
		l.Allow("ip", 1, t0)
	}
	// The wall clock steps back 45 s, as WSL2's time sync does every 30 s
	// (libretto's case). Neither a reset nor a refill may follow.
	for _, back := range []time.Duration{-time.Millisecond, -45 * time.Second, -time.Hour} {
		if l.Allow("ip", 1, t0.Add(back)) {
			t.Errorf("a %v clock step let a request through an empty bucket", back)
		}
	}
	if l.Allow("ip", 1, t0.Add(-30*time.Second)) {
		t.Error("the bucket refilled while the clock was behind its last use")
	}
	if l.Len() != 1 {
		t.Errorf("the step swept a bucket that was not full: %d left", l.Len())
	}
	// Back past the last use, the refill resumes from there.
	if !l.Allow("ip", 1, t0.Add(time.Second)) || l.Allow("ip", 1, t0.Add(time.Second)) {
		t.Error("one second after the last use should give back one token")
	}
}

func TestLimiterSweep(t *testing.T) {
	l := NewLimiter(10, 20) // full again 2 s after its last use
	for i := range 1000 {
		l.Allow(strconv.Itoa(i), 1, t0)
	}
	l.Allow("busy", 20, t0.Add(1500*time.Millisecond))
	if l.Len() != 1001 {
		t.Fatalf("%d buckets before the sweep", l.Len())
	}
	// At 2.5 s the others are full again, so they go; busy is not.
	if !l.Allow("new", 1, t0.Add(2500*time.Millisecond)) {
		t.Fatal("a new key was refused")
	}
	if l.Len() != 2 {
		t.Errorf("%d buckets after the sweep, want busy and new", l.Len())
	}
	if l.Allow("busy", 15, t0.Add(2500*time.Millisecond)) {
		t.Error("the sweep forgot what busy had used")
	}
}

func TestLimiterConcurrent(t *testing.T) {
	l := NewLimiter(1, 100)
	now := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if l.Allow("k", 1, now) {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if allowed != 100 {
		t.Errorf("%d of 400 allowed, want the burst of 100", allowed)
	}
}

func TestNewLimiterRefusesNonsense(t *testing.T) {
	for _, c := range []struct {
		rate  float64
		burst int
	}{{0, 1}, {-1, 1}, {1, 0}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewLimiter(%v, %d) did not panic", c.rate, c.burst)
				}
			}()
			NewLimiter(c.rate, c.burst)
		}()
	}
}

func TestWindow(t *testing.T) {
	l := NewWindow(5, time.Hour)
	for i := range 5 {
		if !l.Allow("ip", 1, t0.Add(time.Duration(i)*10*time.Minute)) {
			t.Fatalf("message %d of 5 refused", i+1)
		}
	}
	// A bucket of 5 an hour would have given a token back by now.
	for _, at := range []time.Duration{40 * time.Minute, 59*time.Minute + 59*time.Second} {
		if l.Allow("ip", 1, t0.Add(at)) {
			t.Errorf("a sixth message at %v", at)
		}
	}
	if !l.Allow("other", 5, t0) {
		t.Error("keys share a window")
	}
	// The next window opens with the first use after the hour, and runs an
	// hour from there.
	next := t0.Add(90 * time.Minute)
	if !l.Allow("ip", 5, next) || l.Allow("ip", 1, next.Add(59*time.Minute)) || !l.Allow("ip", 1, next.Add(time.Hour)) {
		t.Error("the second window")
	}
	if l.Allow("ip", 6, next.Add(3*time.Hour)) || !l.Allow("ip", 5, next.Add(3*time.Hour)) {
		t.Error("more than n at once was allowed, or the refusal took tokens")
	}
	if !l.Allow("ip", 0, next.Add(3*time.Hour)) {
		t.Error("taking nothing is always allowed")
	}
}

func TestWindowSurvivesAClockStep(t *testing.T) {
	l := NewWindow(5, time.Minute)
	l.Allow("ip", 5, t0)
	for _, back := range []time.Duration{-time.Millisecond, -45 * time.Second, -time.Hour} {
		if l.Allow("ip", 1, t0.Add(back)) {
			t.Errorf("a %v clock step opened a new window", back)
		}
	}
	if l.Len() != 1 {
		t.Errorf("the step swept an open window: %d left", l.Len())
	}
	if !l.Allow("ip", 5, t0.Add(time.Minute)) {
		t.Error("the window did not close once the clock passed its end")
	}
}

func TestWindowSweep(t *testing.T) {
	l := NewWindow(3, 2*time.Second)
	for i := range 1000 {
		l.Allow(strconv.Itoa(i), 1, t0)
	}
	l.Allow("busy", 3, t0.Add(1500*time.Millisecond))
	if !l.Allow("new", 1, t0.Add(2500*time.Millisecond)) {
		t.Fatal("a new key was refused")
	}
	if l.Len() != 2 {
		t.Errorf("%d windows after the sweep, want busy and new", l.Len())
	}
	if l.Allow("busy", 1, t0.Add(2500*time.Millisecond)) {
		t.Error("the sweep closed busy's window early")
	}
}

func TestWindowConcurrent(t *testing.T) {
	l := NewWindow(100, time.Hour)
	now := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if l.Allow("k", 1, now) {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	if allowed != 100 {
		t.Errorf("%d of 400 allowed, want the window's 100", allowed)
	}
}

func TestNewWindowRefusesNonsense(t *testing.T) {
	for _, c := range []struct {
		n   int
		per time.Duration
	}{{0, time.Hour}, {-1, time.Hour}, {1, 0}, {1, -time.Second}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewWindow(%d, %v) did not panic", c.n, c.per)
				}
			}()
			NewWindow(c.n, c.per)
		}()
	}
}

// Five messages in the last hour: a token comes back exactly an hour after
// it was taken, wherever a fixed window would have its edge.
func TestSliding(t *testing.T) {
	l := NewSliding(5, time.Hour)
	late := 59*time.Minute + 30*time.Second
	if !l.Allow("ip", 1, t0) || !l.Allow("ip", 4, t0.Add(late)) {
		t.Fatal("five messages within the hour were refused")
	}
	// A fixed window opened at t0 would give five more from t0+1h.
	for _, at := range []time.Duration{late + time.Second, time.Hour - time.Nanosecond} {
		if l.Allow("ip", 1, t0.Add(at)) {
			t.Errorf("a sixth message at %v", at)
		}
	}
	if !l.Allow("ip", 1, t0.Add(time.Hour)) || l.Allow("ip", 1, t0.Add(time.Hour)) {
		t.Error("an hour after the first message, exactly one more")
	}
	if l.Allow("ip", 1, t0.Add(late+time.Hour-time.Nanosecond)) || !l.Allow("ip", 4, t0.Add(late+time.Hour)) {
		t.Error("the four of", late, "came back early or late")
	}
	if !l.Allow("other", 5, t0) {
		t.Error("keys share a window")
	}
	if l.Allow("other", 6, t0.Add(3*time.Hour)) || !l.Allow("other", 5, t0.Add(3*time.Hour)) {
		t.Error("more than n at once was allowed, or the refusal took tokens")
	}
	if !l.Allow("other", 0, t0.Add(3*time.Hour)) {
		t.Error("taking nothing is always allowed")
	}
}

// In every span of per a sliding window lets at most n through. Up to 64 it
// decides as a log of every use would; above, a token comes back at most
// per/64 late.
func TestSlidingPromise(t *testing.T) {
	const per = time.Hour
	type taken struct {
		at time.Time
		n  int
	}
	for _, n := range []int{1, 5, 64, 65, 500} {
		l := NewSliding(n, per)
		r := rand.New(rand.NewPCG(uint64(n), 1))
		var log []taken // what l gave within the last per and a 64th
		within := func(now time.Time, span time.Duration) (sum int) {
			for _, u := range log {
				if now.Sub(u.at) < span {
					sum += u.n
				}
			}
			return sum
		}
		now := t0
		for range 20000 {
			gap := time.Duration(r.Int64N(int64(4 * per / time.Duration(n))))
			if r.IntN(100) == 0 {
				gap = time.Duration(r.Int64N(int64(2 * per)))
			}
			now = now.Add(gap)
			for len(log) > 0 && now.Sub(log[0].at) >= per+per/grains {
				log = log[1:]
			}
			k := 1 + r.IntN(3)
			got, exact := l.Allow("k", k, now), within(now, per)+k <= n
			switch {
			case got && !exact:
				t.Fatalf("n=%d: gave %d at %v with %d in the last %v", n, k, now.Sub(t0), within(now, per), per)
			case !got && exact && n <= grains:
				t.Fatalf("n=%d: refused %d at %v with %d in the last %v", n, k, now.Sub(t0), within(now, per), per)
			case !got && within(now, per+per/grains)+k <= n:
				t.Fatalf("n=%d: refused %d at %v with %d in the last %v", n, k, now.Sub(t0), within(now, per+per/grains), per+per/grains)
			}
			if got {
				log = append(log, taken{now, k})
			}
		}
	}
}

// A key keeps at most n uses, and 65 for an n over 64. A key whose uses
// are all older than per is swept.
func TestSlidingMemory(t *testing.T) {
	for _, c := range []struct{ n, most int }{{5, 5}, {64, 64}, {1000, 65}} {
		l := NewSliding(c.n, time.Minute)
		for i := range 10 * c.n {
			l.Allow("k", 1, t0.Add(time.Duration(i)*time.Minute/time.Duration(c.n)))
		}
		if got := len(l.buckets["k"].uses); got > c.most {
			t.Errorf("NewSliding(%d) keeps %d uses for a key, want at most %d", c.n, got, c.most)
		}
	}

	l := NewSliding(3, 2*time.Second)
	for i := range 1000 {
		l.Allow(strconv.Itoa(i), 1, t0)
	}
	l.Allow("busy", 3, t0.Add(1500*time.Millisecond))
	if !l.Allow("new", 1, t0.Add(2500*time.Millisecond)) {
		t.Fatal("a new key was refused")
	}
	if l.Len() != 2 {
		t.Errorf("%d keys after the sweep, want busy and new", l.Len())
	}
	if l.Allow("busy", 1, t0.Add(2500*time.Millisecond)) {
		t.Error("the sweep forgot what busy took")
	}
}

func TestSlidingSurvivesAClockStep(t *testing.T) {
	l := NewSliding(5, time.Minute)
	l.Allow("ip", 5, t0)
	for _, back := range []time.Duration{-time.Millisecond, -45 * time.Second, -time.Hour} {
		if l.Allow("ip", 1, t0.Add(back)) {
			t.Errorf("a %v clock step gave a token back", back)
		}
	}
	if l.Len() != 1 {
		t.Errorf("the step swept a key with uses: %d left", l.Len())
	}
	if !l.Allow("ip", 5, t0.Add(time.Minute)) {
		t.Error("the uses still count once the clock is per past them")
	}
}

func TestSlidingConcurrent(t *testing.T) {
	l := NewSliding(100, time.Hour)
	now := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if l.Allow("k", 1, now) {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	if allowed != 100 {
		t.Errorf("%d of 400 allowed, want the window's 100", allowed)
	}
}

func TestNewSlidingRefusesNonsense(t *testing.T) {
	for _, c := range []struct {
		n   int
		per time.Duration
	}{{0, time.Hour}, {-1, time.Hour}, {1, 0}, {1, -time.Second}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewSliding(%d, %v) did not panic", c.n, c.per)
				}
			}()
			NewSliding(c.n, c.per)
		}()
	}
}
