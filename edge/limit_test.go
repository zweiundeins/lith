package edge

import (
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
