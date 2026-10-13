package live

import (
	"hash/maphash"
	"math/bits"
	"math/rand/v2"
	"sync"
)

// Cache keeps rendered fragments by a key that stands for what they show
// (content-addressed: a card's id, version and day), bounded in bytes. A
// fragment whose key comes again is copied instead of rendered.
//
// It admits by frequency, after W-TinyLFU (Caffeine): a new fragment
// waits in a small window, and enters the main cache only if its key has
// been asked for more often than the fragment it would push out. Keys
// seen once (a component whose input always changes) never displace the
// ones that come back every tick, and fragments nobody asks for any more
// lose their place to ones that are. Safe for concurrent use.
type Cache[K comparable] struct {
	mu        sync.Mutex
	seed      maphash.Seed
	entries   map[K]*entry[K]
	window    list[K] // new fragments, least recently used at the back
	probation list[K] // admitted, not yet asked for again
	protected list[K] // asked for again since admission
	windowMax int
	mainMax   int // probation and protected together
	protMax   int
	itemMax   int // larger fragments are not kept
	sketch    sketch
	rnd       *rand.Rand // admits a warm candidate that ties, as Caffeine does
	stats     CacheStats
}

// CacheStats count a cache's work.
type CacheStats struct {
	Hits, Misses int64
	Rejected     int64 // fragments the admission turned away
	Entries      int
	Bytes        int
}

// entryOverhead is the memory an entry costs besides its fragment.
const entryOverhead = 96

// NewCache returns a cache that holds at most maxBytes of fragments.
func NewCache[K comparable](maxBytes int) *Cache[K] {
	window := max(maxBytes/100, 1)
	main := maxBytes - window
	return &Cache[K]{
		seed:      maphash.MakeSeed(),
		entries:   map[K]*entry[K]{},
		windowMax: window,
		mainMax:   main,
		protMax:   main * 4 / 5,
		itemMax:   max(maxBytes/16, 1),
		sketch:    newSketch(maxBytes / 512),
		rnd:       rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
	}
}

// Get returns the fragment for key, from the cache or else from render.
// Render runs without a lock held, and must return the same bytes for the
// same key; the caller must not change them.
func (c *Cache[K]) Get(key K, render func() []byte) []byte {
	h := maphash.Comparable(c.seed, key)
	c.mu.Lock()
	c.sketch.add(h)
	if e := c.entries[key]; e != nil {
		c.stats.Hits++
		c.touch(e)
		c.mu.Unlock()
		return e.value
	}
	c.stats.Misses++
	c.mu.Unlock()

	value := render()
	value = value[:len(value):len(value)] // an append by one caller must copy
	size := len(value) + entryOverhead
	if size > c.itemMax {
		return value
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[key]; e != nil {
		return e.value
	}
	e := &entry[K]{key: key, hash: h, value: value, size: size, where: inWindow}
	c.entries[key] = e
	c.window.pushFront(e)
	for c.window.bytes > c.windowMax {
		c.admit(c.window.back())
	}
	return value
}

// Stats reads the counters.
func (c *Cache[K]) Stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Entries = len(c.entries)
	s.Bytes = c.window.bytes + c.probation.bytes + c.protected.bytes
	return s
}

// touch records a hit: a fragment asked for again in probation moves to
// protected, which hands its least recent ones back to probation.
func (c *Cache[K]) touch(e *entry[K]) {
	switch e.where {
	case inWindow:
		c.window.moveToFront(e)
	case inProtected:
		c.protected.moveToFront(e)
	case inProbation:
		c.probation.remove(e)
		e.where = inProtected
		c.protected.pushFront(e)
		for c.protected.bytes > c.protMax {
			d := c.protected.back()
			c.protected.remove(d)
			d.where = inProbation
			c.probation.pushFront(d)
		}
	}
}

// admit moves the window's least recent fragment into the main cache if
// there is room, or if its key is asked for more often than the keys of
// the fragments it would push out; otherwise it is dropped.
func (c *Cache[K]) admit(cand *entry[K]) {
	c.window.remove(cand)
	for c.probation.bytes+c.protected.bytes+cand.size > c.mainMax {
		victim := c.probation.back()
		if victim == nil {
			victim = c.protected.back()
		}
		if victim == nil || !c.wins(cand, victim) {
			delete(c.entries, cand.key)
			c.stats.Rejected++
			return
		}
		c.evict(victim)
	}
	cand.where = inProbation
	c.probation.pushFront(cand)
}

// wins reports whether cand may push victim out. Counters saturate at 15
// and collide, so a fragment that keeps coming back can tie with one asked
// for once; a warm candidate that does not win outright gets in one time
// in 128, which Caffeine uses for the same reason.
func (c *Cache[K]) wins(cand, victim *entry[K]) bool {
	cf, vf := c.sketch.estimate(cand.hash), c.sketch.estimate(victim.hash)
	if cf > vf {
		return true
	}
	return cf >= 6 && c.rnd.IntN(128) == 0
}

func (c *Cache[K]) evict(e *entry[K]) {
	if e.where == inProtected {
		c.protected.remove(e)
	} else {
		c.probation.remove(e)
	}
	delete(c.entries, e.key)
}

const (
	inWindow = iota
	inProbation
	inProtected
)

type entry[K comparable] struct {
	key        K
	hash       uint64
	value      []byte
	size       int
	where      int
	prev, next *entry[K]
}

// list is a doubly linked list of entries that keeps their total size.
type list[K comparable] struct {
	head, tail *entry[K]
	bytes      int
}

func (l *list[K]) pushFront(e *entry[K]) {
	e.prev, e.next = nil, l.head
	if l.head != nil {
		l.head.prev = e
	} else {
		l.tail = e
	}
	l.head = e
	l.bytes += e.size
}

func (l *list[K]) remove(e *entry[K]) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		l.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		l.tail = e.prev
	}
	e.prev, e.next = nil, nil
	l.bytes -= e.size
}

func (l *list[K]) moveToFront(e *entry[K]) {
	if l.head != e {
		l.remove(e)
		l.pushFront(e)
	}
}

func (l *list[K]) back() *entry[K] { return l.tail }

// sketch is a count-min sketch of how often keys were asked for: four
// rows of 4-bit counters. All counters are halved after every 10 x width
// additions, so the estimate follows what is popular now.
type sketch struct {
	rows    [4][]uint64 // 16 counters per word
	mask    uint64
	added   int
	resetAt int
}

func newSketch(width int) sketch {
	width = 1 << bits.Len(uint(max(width, 64)-1)) // a power of two, at least 64
	var s sketch
	for i := range s.rows {
		s.rows[i] = make([]uint64, width/16)
	}
	s.mask = uint64(width - 1)
	s.resetAt = 10 * width
	return s
}

// slot is the word and shift of a key's counter in row i.
func (s *sketch) slot(h uint64, i int) (int, uint) {
	h = (h ^ uint64(i)*0x9e3779b97f4a7c15) * 0xbf58476d1ce4e5b9
	h ^= h >> 31
	idx := h & s.mask
	return int(idx / 16), uint(idx%16) * 4
}

func (s *sketch) add(h uint64) {
	for i := range s.rows {
		w, sh := s.slot(h, i)
		if (s.rows[i][w]>>sh)&0xf < 15 {
			s.rows[i][w] += 1 << sh
		}
	}
	if s.added++; s.added >= s.resetAt {
		for i := range s.rows {
			for j, word := range s.rows[i] {
				s.rows[i][j] = (word >> 1) & 0x7777777777777777
			}
		}
		s.added /= 2
	}
}

func (s *sketch) estimate(h uint64) uint64 {
	n := uint64(15)
	for i := range s.rows {
		w, sh := s.slot(h, i)
		n = min(n, (s.rows[i][w]>>sh)&0xf)
	}
	return n
}
