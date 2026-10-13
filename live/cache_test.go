package live

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

func fragment(key, size int) func() []byte {
	return func() []byte { return bytes.Repeat([]byte{byte(key)}, size) }
}

func TestCacheHits(t *testing.T) {
	c := NewCache[string](1 << 20)
	renders := 0
	render := func() []byte { renders++; return []byte("<li>a</li>") }
	for range 3 {
		if got := c.Get("a", render); string(got) != "<li>a</li>" {
			t.Fatalf("got %q", got)
		}
	}
	if s := c.Stats(); renders != 1 || s.Hits != 2 || s.Misses != 1 || s.Entries != 1 {
		t.Fatalf("%d renders, %+v", renders, s)
	}
}

// However many fragments come, the cache holds at most its size.
func TestCacheIsBounded(t *testing.T) {
	const max = 64 << 10
	c := NewCache[int](max)
	for i := range 10000 {
		c.Get(i%3000, fragment(i, 100+i%700))
		if b := c.Stats().Bytes; b > max {
			t.Fatalf("%d bytes after %d fragments", b, i)
		}
	}
}

// Fragments asked for once do not push out the ones asked for all the
// time, not even those that overflowed the protected part of the cache
// and wait in probation, where a plain segmented LRU would drop them.
func TestCacheResistsScans(t *testing.T) {
	c := NewCache[int](100 << 10)
	const hot = 150 // 89 kB: more than protected (79 kB), less than the cache
	for range 10 {
		for k := range hot {
			c.Get(k, fragment(k, 500))
		}
	}
	before := c.Stats()
	for round := range 100 {
		for k := range 100 {
			c.Get(1000+round*100+k, fragment(k, 500))
		}
		for k := range hot {
			c.Get(k, fragment(k, 500))
		}
	}
	s := c.Stats()
	hits := s.Hits - before.Hits
	t.Logf("%d of %d asks for hot fragments were hits", hits, 100*hot)
	if hits < 100*hot*97/100 {
		t.Fatalf("%d of %d asks for hot fragments were hits", hits, 100*hot)
	}
}

// A fragment that keeps being asked for gets in, even into a full cache.
func TestCacheAdmitsWhatComesBack(t *testing.T) {
	c := NewCache[int](20 << 10)
	for k := range 200 {
		c.Get(k, fragment(k, 500))
	}
	// A tie with a victim whose counters collide with hot keys is broken at
	// random, one time in 128, so give it rounds enough for that.
	for k := 1000; k < 1000+4000*10; k += 10 {
		before := c.Stats().Hits
		c.Get(-1, fragment(1, 500))
		if c.Stats().Hits > before {
			return
		}
		for j := k; j < k+10; j++ {
			c.Get(j, fragment(j, 500))
		}
	}
	t.Fatal("a fragment asked for every round never got in")
}

func TestCacheSkipsLargeFragments(t *testing.T) {
	c := NewCache[int](16 << 10)
	c.Get(1, fragment(1, 2<<10))
	if s := c.Stats(); s.Entries != 0 {
		t.Fatalf("kept a fragment of an eighth of the cache: %+v", s)
	}
}

func TestSketchAges(t *testing.T) {
	s := newSketch(64)
	for range 12 {
		s.add(42)
	}
	if n := s.estimate(42); n != 12 {
		t.Fatalf("estimate %d", n)
	}
	for i := range s.resetAt {
		s.add(uint64(1000 + i%7))
	}
	if n := s.estimate(42); n > 6 {
		t.Fatalf("estimate %d after the counters were halved", n)
	}
}

func TestCacheConcurrent(t *testing.T) {
	c := NewCache[string](32 << 10)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 2000 {
				k := fmt.Sprint((i * (w + 1)) % 300)
				if got := c.Get(k, func() []byte { return []byte(k) }); string(got) != k {
					t.Errorf("%q for %q", got, k)
					return
				}
			}
		})
	}
	wg.Wait()
	if s := c.Stats(); s.Bytes > 32<<10 || s.Hits == 0 {
		t.Fatalf("%+v", s)
	}
}
