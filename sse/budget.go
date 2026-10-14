package sse

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ErrNoRoom is returned by Open when the budget has no room for the
// stream and BudgetOptions.Refuse is set. Open has written nothing: the
// handler answers 503, and a page that opened the stream with retry
// 'error' tries again (live's Serve answers it).
var ErrNoRoom = errors.New("sse: no room in the streams' budget")

// BudgetOptions configure a Budget. The zero value takes half the
// process's memory limit and windows of 2^18 to 2^19.
type BudgetOptions struct {
	// Limit is the heap, in bytes, that the encoders of the streams may
	// hold together. Zero takes half the process's memory limit:
	// GOMEMLIMIT, else the cgroup's memory.max or memory.high, whichever is
	// lower. Without one of them, or with a negative Limit, there is no
	// limit: every stream gets the largest tier, and the budget only counts.
	Limit int64
	// MinWindow is the smallest window, as a base 2 logarithm, that a
	// stream compresses with (default 18, for frames up to about 250 KB).
	// Set it to hold the app's largest frame: below that a frame cannot
	// refer to the one before, and the bytes per frame grow several times
	// over (a 142 KB board: from 2.7 KB at 2^18 to 10.8 KB at 2^17). A
	// zstd stream that cannot have it sends raw blocks until it can. A
	// brotli stream keeps its tier until it closes, so below MinWindow it
	// takes quality 3 with smaller windows, down to 2^16 (10 KB for that
	// board instead of 142 KB uncompressed). A stream that cannot have even
	// its smallest tier goes out uncompressed, or with Refuse not at all.
	MinWindow int
	// MaxWindow is the largest window a stream gets (default 19, the
	// window of Options' defaults for zstd).
	MaxWindow int
	// Refuse turns away a stream that cannot have its smallest tier (zstd:
	// MinWindow's): Open returns ErrNoRoom. Without it the stream goes out
	// uncompressed, which suits small, rare frames; large and frequent
	// frames are better refused, for pages that open their stream with
	// retry 'error' and so come back.
	Refuse bool
}

// Budget shares a memory limit among the encoders of the streams that open
// with it through Options.Budget, so that a server holds what its memory
// allows, however many tabs are open: few streams compress with large
// windows, many with small ones.
//
// A stream reserves the heap of its encoder when it opens: the largest tier
// that fits both its fair share (the limit divided by the open streams that
// compress or would, itself included) and what is left of the limit. Tiers
// are zstd's and brotli's windows from MaxWindow down to MinWindow; zstd's
// largest keeps its full history, and brotli's go on below MinWindow at
// quality 3, down to 2^16. A stream whose fair share is below its smallest
// tier gets that tier while what is left holds it. Close gives
// the reservation back, and so does a zstd stream that hibernates
// (Stream.Hibernate): it reserves again on its next write, at the fair
// share of that moment.
//
// Streams that open while few are open get large tiers, and would keep
// them as more arrive. So when a stream finds no room, every zstd stream
// above the fair share of that moment (counting a refused stream as open)
// moves down at its next write: it ends its zstd frame and begins the next
// with the smaller tier, which costs one frame without history. Brotli and
// gzip streams keep their tier until they close, since a brotli body is
// one stream with one window: brotli steps down as streams open instead,
// from its quality's windows to quality 3 at 2^16. Once more than 20
// streams are open, a brotli stream takes at most its largest tier times twice
// the share of the limit that is left: the largest tier while half the
// limit is left, smaller ones as it fills, so that a tier taken early does
// not spend the room of the streams to come.
//
// The tiers' sizes were measured with zstd at its fastest level and brotli
// at quality 3 and 4 (lith's bench/budget), after a session of 142 KB
// frames, and are rounded up. Other levels and qualities hold more. A
// brotli stream flushes its encoder every 16 KiB of a frame at quality 3
// and every 32 KiB at 4, which keeps it below its tier at any frame size
// (bench/budget/framesize.csv).
type Budget struct {
	limit                int64 // 0: none
	minWindow, maxWindow int
	refuse               bool

	mu       sync.Mutex
	reserved int64
	users    int            // open streams that compress or would
	streams  map[string]int // open streams by what they hold
	refused  int64
	starved  atomic.Int64 // counts the times a stream found no room; changed with mu held
}

// NewBudget returns a budget for streams to share through Options.Budget.
func NewBudget(o BudgetOptions) *Budget {
	if o.Limit == 0 {
		o.Limit = memoryLimit() / 2
	}
	if o.MinWindow <= 0 {
		o.MinWindow = 18
	}
	if o.MaxWindow <= 0 {
		o.MaxWindow = 19
	}
	return &Budget{
		limit:     max(0, o.Limit),
		minWindow: o.MinWindow,
		maxWindow: max(o.MinWindow, o.MaxWindow),
		refuse:    o.Refuse,
		streams:   map[string]int{},
	}
}

// BudgetStats describe a budget at one moment.
type BudgetStats struct {
	Limit    int64 // bytes, 0 for none
	Reserved int64 // bytes the open streams hold
	// Streams counts the open streams by what they hold: a tier such as
	// "zstd 2^19", "zstd 2^17 low memory" or "br q4 2^18"; "zstd asleep"
	// for zstd streams without an encoder (hibernating, or waiting for
	// room); "identity" for streams that go out uncompressed.
	Streams map[string]int
	Refused int64 // streams turned away since NewBudget
}

// Stats reads the budget.
func (b *Budget) Stats() BudgetStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := BudgetStats{Limit: b.limit, Reserved: b.reserved, Refused: b.refused, Streams: make(map[string]int, len(b.streams))}
	for k, n := range b.streams {
		s.Streams[k] = n
	}
	return s
}

// SetLimit changes the budget's limit in bytes (0 for none), as when the
// process nears its memory limit. The zstd streams above their fair share
// under the new limit move down at their next write, or, when it leaves no
// room for them, send their frames uncompressed until it does. Brotli and
// gzip streams keep their tiers until they close.
func (b *Budget) SetLimit(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.limit = max(0, n)
	b.starved.Add(1)
}

// tier is a setting a budget hands out and the heap a stream holds with it.
type tier struct {
	coding  string // "" for none
	window  int    // base 2 logarithm
	lowMem  bool   // zstd's history at its window
	quality int    // brotli's
	heap    int64
}

func (t tier) String() string {
	switch t.coding {
	case Zstd:
		if t.lowMem {
			return fmt.Sprintf("zstd 2^%d low memory", t.window)
		}
		return fmt.Sprintf("zstd 2^%d", t.window)
	case Brotli:
		return fmt.Sprintf("br q%d 2^%d", t.quality, t.window)
	}
	return t.coding
}

const kib = 1 << 10

// The tiers, largest first: the heap of a stream after a session of 142 KB
// frames (bench/budget/settings.csv, the board), rounded up to 16 KiB.
// Brotli's were measured before its streams flushed within a frame, and
// book 12 to 32% more than the streams now hold (bench/budget/framesize.csv).
// zstd with its full history holds at least 1 MiB of it at any window, so
// below 2^19 only its low-memory tiers are worth their heap.
var (
	zstdTiers = []tier{
		{coding: Zstd, window: 20, heap: 2720 * kib},
		{coding: Zstd, window: 19, heap: 1696 * kib},
		{coding: Zstd, window: 20, lowMem: true, heap: 1616 * kib},
		{coding: Zstd, window: 19, lowMem: true, heap: 1104 * kib},
		{coding: Zstd, window: 18, lowMem: true, heap: 848 * kib},
		{coding: Zstd, window: 17, lowMem: true, heap: 720 * kib},
		{coding: Zstd, window: 16, lowMem: true, heap: 656 * kib},
		{coding: Zstd, window: 15, lowMem: true, heap: 544 * kib},
	}
	brotliTiers = map[int][]tier{
		3: {
			{coding: Brotli, quality: 3, window: 20, heap: 2848 * kib},
			{coding: Brotli, quality: 3, window: 19, heap: 1824 * kib},
			{coding: Brotli, quality: 3, window: 18, heap: 1312 * kib},
			{coding: Brotli, quality: 3, window: 17, heap: 1056 * kib},
			{coding: Brotli, quality: 3, window: 16, heap: 912 * kib},
		},
		4: {
			{coding: Brotli, quality: 4, window: 20, heap: 3728 * kib},
			{coding: Brotli, quality: 4, window: 19, heap: 2704 * kib},
			{coding: Brotli, quality: 4, window: 18, heap: 2192 * kib},
			{coding: Brotli, quality: 4, window: 17, heap: 1936 * kib},
			{coding: Brotli, quality: 4, window: 16, heap: 1792 * kib},
		},
	}
	gzipTier = tier{coding: Gzip, window: 15, heap: 832 * kib}
)

// menu lists the tiers a stream of the coding may get, largest first.
func (b *Budget) menu(coding string, o Options) []tier {
	switch coding {
	case Brotli:
		return b.brotliMenu(o)
	case Gzip:
		return []tier{gzipTier}
	}
	// The stream's own window is the largest it may get, and lowers the
	// floor with it, so that a budget without a limit changes nothing.
	lo, hi := min(b.minWindow, o.ZstdWindow), min(b.maxWindow, o.ZstdWindow)
	var m []tier
	for _, t := range zstdTiers {
		if t.window >= lo && t.window <= hi && (t.lowMem || !o.ZstdLowMemory) {
			m = append(m, t)
		}
	}
	return m
}

// brotliMenu lists a brotli stream's tiers, largest first: its quality's
// windows from MaxWindow down to MinWindow (qualities other than 3 are
// costed as 4), then quality 3's that hold less, down to 2^16. A brotli
// stream keeps its tier until it closes, so a small window beats none.
func (b *Budget) brotliMenu(o Options) []tier {
	q := o.BrotliQuality
	all := brotliTiers[4]
	if q == 3 {
		all = brotliTiers[3]
	}
	ceil := o.BrotliWindow
	lo, hi := min(b.minWindow, ceil), min(b.maxWindow, ceil)
	var m []tier
	for _, t := range all {
		if t.window >= lo && t.window <= hi {
			t.quality = q
			m = append(m, t)
		}
	}
	for _, t := range brotliTiers[3] {
		if t.window <= hi && (len(m) == 0 || t.heap < m[len(m)-1].heap && t.window <= m[len(m)-1].window) {
			m = append(m, t)
		}
	}
	return m
}

// quietStreams is how many open streams a server may have and still give
// a brotli stream its largest tier wherever the fair share and what is
// left allow it (simulated in TestBudgetCurve).
const quietStreams = 20

// pick chooses the largest tier of the coding that fits the fair share
// among users and what is left of the limit, and for brotli past
// quietStreams the curve on what is left (see Budget), or else the
// smallest one if what is left holds it. b.mu is held.
func (b *Budget) pick(coding string, o Options, users int) (tier, bool) {
	m := b.menu(coding, o)
	if len(m) == 0 {
		return tier{}, false
	}
	if b.limit == 0 {
		return m[0], true
	}
	left := b.limit - b.reserved
	fit := min(left, b.limit/int64(users))
	if coding == Brotli && users > quietStreams {
		fit = min(fit, m[0].heap*2*left/b.limit)
	}
	for _, t := range m {
		if t.heap <= fit {
			return t, true
		}
	}
	if t := m[len(m)-1]; t.heap <= left {
		return t, true
	}
	b.starved.Add(1)
	return tier{}, false
}

// label names what a stream of the coding holds, for Stats.
func label(t tier, coding string) string {
	switch {
	case t.coding != "":
		return t.String()
	case coding == Zstd:
		return "zstd asleep"
	}
	return "identity"
}

// open counts a stream that opens with the coding the client accepts, ""
// for none, and reserves its tier: the zero tier when it compresses
// nothing. user reports whether the stream counts in the fair share, ok
// false that the budget refuses it. seen gets the count of streams that
// found no room, for shrink.
func (b *Budget) open(coding string, o Options, seen *int64) (t tier, user, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	defer func() { *seen = b.starved.Load() }()
	if coding != "" {
		if t, ok = b.pick(coding, o, b.users+1); !ok && b.refuse {
			b.refused++
			return tier{}, false, false
		}
		b.users++
		user = true
	}
	if t.coding == "" && coding != Zstd {
		coding = ""
	}
	b.hold(t, coding)
	return t, user, true
}

// wake reserves a tier for a zstd stream without one, if it fits.
func (b *Budget) wake(o Options, seen *int64) (tier, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.pick(Zstd, o, b.users)
	*seen = b.starved.Load()
	if ok {
		b.drop(tier{}, Zstd)
		b.hold(t, Zstd)
	}
	return t, ok
}

// shrink moves a zstd stream that holds t down to the largest tier within
// its fair share, when a stream found no room since the stream's last tier
// (or the limit changed) and the share is smaller than t. The move lowers
// what the streams hold, so it does not wait for what is left: after the
// limit dropped, the streams above their share hold more than the limit
// until they have all moved. A share below the smallest tier gets that tier
// while what is left holds it, and none after that. A budget that refuses
// streams counts the one it turned away, which is not open. It reports the
// new tier, or false to keep t.
func (b *Budget) shrink(t tier, o Options, seen *int64) (tier, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	*seen = b.starved.Load()
	users := b.users
	if b.refuse {
		users++
	}
	share := b.limit / int64(users)
	m := b.menu(Zstd, o)
	if b.limit == 0 || t.heap <= share || len(m) == 0 {
		return t, false
	}
	b.drop(t, Zstd)
	var n tier
	for _, c := range m {
		if c.heap <= share {
			n = c
			break
		}
	}
	if f := m[len(m)-1]; n.coding == "" && f.heap <= b.limit-b.reserved {
		n = f
	}
	b.hold(n, Zstd)
	return n, n != t
}

// sleep gives back the tier of a zstd stream that hibernates.
func (b *Budget) sleep(t tier) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.drop(t, Zstd)
	b.hold(tier{}, Zstd)
}

// close counts out a stream of the coding, "" for none, and gives back its
// tier.
func (b *Budget) close(t tier, coding string, user bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.drop(t, coding)
	if user {
		b.users--
	}
}

// hold and drop count a stream in and out. b.mu is held.
func (b *Budget) hold(t tier, coding string) {
	b.streams[label(t, coding)]++
	b.reserved += t.heap
}

func (b *Budget) drop(t tier, coding string) {
	k := label(t, coding)
	if b.streams[k]--; b.streams[k] <= 0 {
		delete(b.streams, k)
	}
	b.reserved -= t.heap
}

// memoryLimit is the process's memory limit in bytes: GOMEMLIMIT (or
// debug.SetMemoryLimit) when set, else the cgroup's memory.max or
// memory.high, whichever is lower, or 0 when there is none.
func memoryLimit() int64 {
	if l := debug.SetMemoryLimit(-1); l > 0 && l < math.MaxInt64 {
		return l
	}
	return cgroupLimit("/proc/self/cgroup", "/sys/fs/cgroup")
}

// cgroupLimit reads the memory limit of the process's cgroup (v2), 0 for
// none.
func cgroupLimit(self, root string) int64 {
	b, err := os.ReadFile(self)
	if err != nil {
		return 0
	}
	for line := range strings.Lines(string(b)) {
		path, ok := strings.CutPrefix(strings.TrimSpace(line), "0::")
		if !ok {
			continue
		}
		var limit int64
		for _, f := range []string{"memory.max", "memory.high"} {
			b, err := os.ReadFile(filepath.Join(root, path, f))
			if err != nil {
				continue
			}
			n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
			if err == nil && n > 0 && (limit == 0 || n < limit) {
				limit = n
			}
		}
		return limit
	}
	return 0
}
