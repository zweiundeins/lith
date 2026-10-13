package edge

import (
	"slices"
	"sync"
	"time"
)

// Limiter limits what each key may take, as a token bucket (NewLimiter), a
// fixed window (NewWindow) or a sliding one (NewSliding). A bucket that has
// filled up again, or a window that has closed, is the same as a new one, so
// Allow sweeps those out as it goes: memory holds the keys that were limited
// within about the last burst/rate seconds, or the last window.
//
// Keys a client chooses, such as session ids, cost it nothing to change: check
// a per-IP limiter (ClientKey) first, so only requests it lets through make
// buckets in the other.
type Limiter struct {
	mu      sync.Mutex
	rate    float64       // tokens a second, for a bucket
	per     time.Duration // the length of a window, 0 for a bucket
	slide   bool          // the window slides
	grain   time.Duration // a sliding window counts the uses within one grain together
	burst   float64
	every   time.Duration // between sweeps
	swept   time.Time
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time // the last refill, when the window opened, or what a sliding window's uses count from
	uses   []use     // a sliding window's, oldest first
}

// use is n tokens a sliding window gave, at a time since its bucket's at.
// With a grain, at is the end of the grain they were taken in.
type use struct {
	at time.Duration
	n  int
}

// grains is how finely a sliding window of more than grains tokens keeps
// its uses.
const grains = 64

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

// NewWindow returns a limiter that allows n tokens per window of length per.
// A key's window opens with its first use, and the first use once per has
// passed opens the next one, with n tokens again. Where a bucket gives tokens
// back as time passes, a window keeps a promise such as "five messages an
// hour": never more than n in one window. Around the edge between two
// windows, though, up to 2n go through within less than per; NewSliding
// keeps n in every span of per. It panics unless n >= 1 and per > 0.
func NewWindow(n int, per time.Duration) *Limiter {
	if n < 1 || per <= 0 {
		panic("edge: NewWindow needs n >= 1 and per > 0")
	}
	return &Limiter{
		per:     per,
		burst:   float64(n),
		every:   min(max(per, time.Second), time.Minute),
		buckets: map[string]*bucket{},
	}
}

// NewSliding returns a limiter that allows n tokens in any span of length
// per: a token counts until per has passed since it was taken. Where
// NewWindow lets up to 2n through around the edge between two windows, a
// sliding window keeps a promise such as "five messages in the last hour" at
// every moment. A key keeps the uses of the last span, at most n of them; for
// n over 64 it counts the uses within each 64th of per together, from the
// end of that 64th, which bounds a key to 65 uses and gives tokens back up
// to per/64 late. It panics unless n >= 1 and per > 0.
func NewSliding(n int, per time.Duration) *Limiter {
	if n < 1 || per <= 0 {
		panic("edge: NewSliding needs n >= 1 and per > 0")
	}
	l := &Limiter{
		per:     per,
		slide:   true,
		burst:   float64(n),
		every:   min(max(per, time.Second), time.Minute),
		buckets: map[string]*bucket{},
	}
	if n > grains {
		l.grain = max(per/grains, 1)
	}
	return l
}

// Allow takes n tokens from key's bucket or window at now, and reports
// whether it had them. Pass time.Now(): its monotonic reading keeps a step of
// the wall clock from refilling or resetting a bucket. A time without one
// that steps back pauses the refill until the clock is past the bucket's last
// use again, keeps a window open until the clock passes its end, and counts a
// sliding window's uses until the clock is per past them, so the limiter
// turns stricter, never looser.
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
	if l.slide {
		b.take(now.Sub(b.at), n, l.grain)
	}
	return true
}

// Len is the number of buckets held.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

func (l *Limiter) fill(b *bucket, now time.Time) {
	d := now.Sub(b.at)
	switch {
	case l.slide:
		i := 0
		for ; i < len(b.uses) && d-b.uses[i].at >= l.per; i++ {
			b.tokens += float64(b.uses[i].n)
		}
		b.uses = slices.Delete(b.uses, 0, i)
	case l.per > 0:
		if d >= l.per {
			b.tokens, b.at = l.burst, now
		}
	case d > 0:
		b.tokens = min(l.burst, b.tokens+d.Seconds()*l.rate)
		b.at = now
	}
}

// take records n tokens a sliding window gave at d.
func (b *bucket) take(d time.Duration, n int, grain time.Duration) {
	if grain > 0 {
		d = (d/grain + 1) * grain
	}
	// A use at or before the last one's time (the same instant or grain, or
	// a clock that stepped back) joins it, and counts from that later time.
	if last := len(b.uses) - 1; last >= 0 && d <= b.uses[last].at {
		b.uses[last].n += n
		return
	}
	b.uses = append(b.uses, use{at: d, n: n})
}

func (l *Limiter) sweep(now time.Time) {
	for key, b := range l.buckets {
		if l.fill(b, now); b.tokens >= l.burst {
			delete(l.buckets, key)
		}
	}
	l.swept = now
}
