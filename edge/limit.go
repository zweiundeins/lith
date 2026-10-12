package edge

import (
	"sync"
	"time"
)

// Limiter is a token bucket per key: a key holds up to burst tokens and gets
// rate tokens a second back. A bucket that has filled up again is the same as
// a new one, so Allow sweeps those out as it goes: memory holds the keys that
// were limited within about the last burst/rate seconds.
//
// Keys a client chooses, such as session ids, cost it nothing to change: check
// a per-IP limiter (ClientKey) first, so only requests it lets through make
// buckets in the other.
type Limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens a second
	burst   float64
	every   time.Duration // between sweeps
	swept   time.Time
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

// NewLimiter returns a limiter that allows burst tokens at once and rate
// tokens a second after that. It panics unless rate > 0 and burst >= 1.
func NewLimiter(rate float64, burst int) *Limiter {
	if !(rate > 0) || burst < 1 {
		panic("edge: NewLimiter needs rate > 0 and burst >= 1")
	}
	full := time.Duration(float64(burst) / rate * float64(time.Second))
	return &Limiter{
		rate:    rate,
		burst:   float64(burst),
		every:   min(max(full, time.Second), time.Minute),
		buckets: map[string]*bucket{},
	}
}

// Allow takes n tokens from key's bucket at now, and reports whether it had
// them. Pass time.Now(): its monotonic reading keeps a step of the wall clock
// from refilling or resetting a bucket. A time without one that steps back
// pauses the refill until the clock is past the bucket's last use again, so
// the limiter turns stricter, never looser.
func (l *Limiter) Allow(key string, n int, now time.Time) bool {
	if n <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if d := now.Sub(l.swept); d >= l.every || d < 0 {
		l.sweep(now)
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
	}
	l.fill(b, now)
	if b.tokens < float64(n) {
		return false
	}
	b.tokens -= float64(n)
	return true
}

// Len is the number of buckets held.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

func (l *Limiter) fill(b *bucket, now time.Time) {
	if d := now.Sub(b.at); d > 0 {
		b.tokens = min(l.burst, b.tokens+d.Seconds()*l.rate)
		b.at = now
	}
}

func (l *Limiter) sweep(now time.Time) {
	for key, b := range l.buckets {
		if l.fill(b, now); b.tokens >= l.burst {
			delete(l.buckets, key)
		}
	}
	l.swept = now
}
