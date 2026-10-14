package sse

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNoRoom is returned by Open when the budget has no room for the
// stream and BudgetOptions.Refuse is set. Open has written nothing: the
// handler answers 503, and a page that opened the stream with retry
// 'error' tries again (live's Serve answers it).
var ErrNoRoom = errors.New("sse: no room in the streams' budget")

// BudgetOptions configure a Budget. The zero value takes half the
// process's memory limit and gives each stream the window its frames
// need, up to 2^19.
type BudgetOptions struct {
	// Limit is the memory, in bytes, that the streams may hold together:
	// their encoders, 32 KiB per stream for the rest of it (its state, its
	// goroutines' stacks, its connection's buffers in net/http), and what
	// Hold books, such as the frames a live hub keeps for its views. Zero
	// takes half the process's memory limit: GOMEMLIMIT, else the cgroup's
	// memory.max or memory.high, whichever is lower. The other half is for
	// the rest of the app and for the garbage that renders and compression
	// leave between two collections. Without a memory limit, or with a
	// negative Limit, there is no limit: every stream gets the largest tier
	// its options allow, and the budget only counts.
	Limit int64
	// MinWindow, if set, is the smallest window, as a base 2 logarithm,
	// that a stream's frames need. A stream needs a window that holds its
	// largest write with a sixteenth to spare, so that a frame can refer
	// to the one before. Set MinWindow when frames refer back further, as
	// when a page sends the parts of a view in turn, or one view after
	// another on the same stream.
	MinWindow int
	// MaxWindow is the largest window a stream gets (default 19).
	MaxWindow int
	// Refuse turns away a stream that cannot have its smallest tier: Open
	// returns ErrNoRoom. Without it the stream goes out uncompressed, which
	// suits small, rare frames; large and frequent frames are better
	// refused, for pages that open their stream with retry 'error' and so
	// come back.
	Refuse bool
}

// Budget shares a memory limit among the streams that open with it
// through Options.Budget, so that a server holds what its memory allows,
// however many tabs are open.
//
// A stream books what it holds from Open to Close: its encoder's tier, and
// 32 KiB for the rest of it. A live hub books each view's newest frame
// once, however many streams watch it (Hold). A stream's tier is picked at
// its first write, from the largest write it has made: a tier's window
// must hold that frame with a sixteenth to spare, or a frame cannot refer
// to the one before, and its bytes grow tens of times over.
//
// A brotli stream takes the smallest window that holds its frame, since a
// larger one sends no fewer bytes: at its quality (4) while the budget
// stays at most half full, else at quality 3, and only when that does not
// fit what is left, quality 3 at 2^16, which compresses each frame alone.
// It keeps its tier until it closes. Open reserves quality 3 at 2^16 for
// it, and a stream that cannot have even that goes out uncompressed. A
// stream that holds less than its frame needs may be asked to open again
// once room frees up (Stream.Upgrade).
//
// A zstd stream takes the largest tier within its fair share (what the
// limit leaves after frames and streams, divided by the streams that
// compress or would) and what is left, down to the smallest window that
// holds its frame, since zstd's bytes keep falling as its window grows. A
// stream whose fair share is below that gets it while what is left holds
// it, then the smallest window, 2^15, and else none: it sends raw blocks
// until room frees up. When a stream finds no room for a window that holds
// its frame, every zstd stream above the fair share of that moment moves
// down at its next write, but not below its frame's window: it ends its
// zstd frame and begins the next with the smaller tier, which costs one
// frame without history. A zstd stream whose window is below its frame
// moves up the same way once room frees up. Close gives the reservation
// back, and so does a zstd stream that hibernates (Stream.Hibernate).
//
// The tiers' sizes are the most a stream held at each, after sessions of
// frames of 50 to 230 KB (lith's bench/budget, framesize.csv), rounded up
// to 16 KiB: zstd at its fastest level, brotli at quality 3 and 4. Other
// levels and qualities hold more.
type Budget struct {
	limit                int64 // 0: none
	minWindow, maxWindow int
	refuse               bool
	reopenEvery          time.Duration // the least time between two streams asked to open again

	mu       sync.Mutex
	reserved int64          // tiers, streams and what Hold booked
	held     int64          // booked through Hold
	nopen    int            // open streams
	users    int            // open streams that compress or would
	streams  map[string]int // open streams by what they hold
	refused  int64
	reopened int64
	movers   []*mover  // streams a new connection would serve better, oldest first
	asked    time.Time // when a stream was last asked to open again
	timer    *time.Timer
	timerAt  time.Time
	starved  atomic.Int64 // counts the times a stream found no room for its frame's window; changed with mu held
	freed    atomic.Int64 // counts the times room freed up; changed with mu held
}

// NewBudget returns a budget for streams to share through Options.Budget.
func NewBudget(o BudgetOptions) *Budget {
	if o.Limit == 0 {
		o.Limit = memoryLimit() / 2
	}
	if o.MaxWindow <= 0 {
		o.MaxWindow = 19
	}
	return &Budget{
		limit:       max(0, o.Limit),
		minWindow:   max(0, o.MinWindow),
		maxWindow:   max(o.MinWindow, o.MaxWindow),
		refuse:      o.Refuse,
		reopenEvery: time.Second,
		streams:     map[string]int{},
	}
}

// BudgetStats describe a budget at one moment.
type BudgetStats struct {
	Limit int64 // bytes, 0 for none
	// Reserved is what the open streams hold: their tiers, 32 KiB each
	// besides, and Frames.
	Reserved int64
	Frames   int64 // booked through Hold: the frames a live hub keeps
	// Streams counts the open streams by what they hold: a tier such as
	// "zstd 2^19", "zstd 2^17 low memory" or "br q4 2^18"; "zstd asleep"
	// for zstd streams without an encoder (before their first write,
	// hibernating, or waiting for room); "identity" for streams that go
	// out uncompressed.
	Streams  map[string]int
	Refused  int64 // streams turned away since NewBudget
	Reopened int64 // streams asked to open again for a better tier (Stream.Upgrade)
}

// Stats reads the budget.
func (b *Budget) Stats() BudgetStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := BudgetStats{Limit: b.limit, Reserved: b.reserved, Frames: b.held, Refused: b.refused, Reopened: b.reopened,
		Streams: make(map[string]int, len(b.streams))}
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
	b.free()
}

// Hold books n more bytes that the streams hold besides their encoders, or
// gives back -n: a live hub books each view's newest frame, its length and
// 8 KiB, once for all the streams that watch it. What Hold books counts
// against the limit and leaves the encoders less room, but is never
// refused: once it takes the whole limit, streams go out uncompressed.
func (b *Budget) Hold(n int64) {
	if n == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held += n
	b.reserved += n
	if n < 0 {
		b.free()
	} else if b.limit > 0 && b.reserved > b.limit {
		b.starved.Add(1)
	}
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

// holds reports whether the tier's window holds a frame of n bytes with a
// sixteenth to spare. Measured at 2^18, brotli refers to the frame before
// up to a frame of 99% of the window, and zstd's bytes grow as it fills.
func (t tier) holds(n int) bool { return t.coding != "" && int64(n)*16 <= int64(15)<<t.window }

const kib = 1 << 10

// streamHeap is what a stream holds besides its encoder: a hub's state for
// it, its goroutines' stacks and its connection's buffers in net/http
// (bench/budget/viewcost.csv).
const streamHeap = 32 * kib

// The tiers, largest first: the most a stream held at each after sessions
// of frames of 50, 142 and 230 KB (bench/budget/framesize.csv), rounded up
// to 16 KiB. zstd with its full history holds at least 1.6 MiB at any
// window, so below 2^19 only its low-memory tiers are worth their heap.
var (
	zstdTiers = []tier{
		{coding: Zstd, window: 20, heap: 2768 * kib},
		{coding: Zstd, window: 19, heap: 1744 * kib},
		{coding: Zstd, window: 20, lowMem: true, heap: 1648 * kib},
		{coding: Zstd, window: 19, lowMem: true, heap: 1136 * kib},
		{coding: Zstd, window: 18, lowMem: true, heap: 880 * kib},
		{coding: Zstd, window: 17, lowMem: true, heap: 752 * kib},
		{coding: Zstd, window: 16, lowMem: true, heap: 688 * kib},
		{coding: Zstd, window: 15, lowMem: true, heap: 592 * kib},
	}
	brotliTiers = map[int][]tier{
		3: {
			{coding: Brotli, quality: 3, window: 20, heap: 2640 * kib},
			{coding: Brotli, quality: 3, window: 19, heap: 1616 * kib},
			{coding: Brotli, quality: 3, window: 18, heap: 1104 * kib},
			{coding: Brotli, quality: 3, window: 17, heap: 848 * kib},
			{coding: Brotli, quality: 3, window: 16, heap: 720 * kib},
		},
		4: {
			{coding: Brotli, quality: 4, window: 20, heap: 3152 * kib},
			{coding: Brotli, quality: 4, window: 19, heap: 2128 * kib},
			{coding: Brotli, quality: 4, window: 18, heap: 1616 * kib},
			{coding: Brotli, quality: 4, window: 17, heap: 1360 * kib},
			{coding: Brotli, quality: 4, window: 16, heap: 1232 * kib},
		},
	}
	gzipTier = tier{coding: Gzip, window: 15, heap: 848 * kib}
	// brotliFloor is what Open reserves for a brotli stream until its
	// first write: the least it holds, whatever its frames.
	brotliFloor = brotliTiers[3][4]
)

// menu lists the tiers a stream of the coding may get when its largest
// write is n bytes: those whose window holds it, best first, and the last
// resort, the coding's smallest tier, which compresses each frame alone.
// A larger brotli window holds no more of the frame before, so brotli's
// are the window that holds it at the stream's quality, then at 3. When
// no window holds the frame, there are none, and the last resort is all.
// Without a limit, the menu is the largest tier the stream's options allow.
func (b *Budget) menu(coding string, o Options, n int) (fit []tier, last tier) {
	var all []tier
	floor, def := 0, 0
	switch coding {
	case Gzip:
		return []tier{gzipTier}, tier{}
	case Zstd:
		floor, def = 15, 19
		for _, t := range zstdTiers {
			if t.lowMem || !o.ZstdLowMemory {
				all = append(all, t)
			}
		}
	case Brotli:
		floor, def = 16, 18
		all = slices.Clone(brotliTiers[4])
		if o.BrotliQuality == 3 {
			all = slices.Clone(brotliTiers[3])
		}
		for i := range all {
			all[i].quality = o.BrotliQuality // other qualities than 3 cost as 4
		}
	default:
		return nil, tier{}
	}
	own := o.ZstdWindow
	if coding == Brotli {
		own = o.BrotliWindow
	}
	if b.limit == 0 {
		// The stream's own window, or its default, so that a budget without
		// a limit changes nothing.
		hi := min(b.maxWindow, cmp.Or(own, def))
		for _, t := range all {
			if t.window <= hi && t.window >= floor {
				return []tier{t}, tier{}
			}
		}
		return nil, tier{}
	}
	hi := min(b.maxWindow, cmp.Or(own, b.maxWindow))
	if hi < floor {
		return nil, tier{}
	}
	w := max(floor, min(b.minWindow, hi))
	for w < hi && int64(n)*16 > int64(15)<<w {
		w++
	}
	for _, t := range all {
		if t.window >= w && t.window <= hi {
			fit = append(fit, t)
		}
	}
	small := all[len(all)-1]
	if coding == Brotli {
		small = brotliFloor
	}
	if len(fit) == 0 || !fit[len(fit)-1].holds(n) {
		return nil, small
	}
	if coding == Brotli {
		fit = fit[len(fit)-1:]
		if fit[0].quality != 3 {
			fit = append(fit, brotliTiers[3][20-w])
		}
	}
	if small.window < w {
		last = small
	}
	return fit, last
}

// share is a stream's fair share of what the limit leaves after frames and
// streams, among users streams. b.mu is held.
func (b *Budget) share(users int) int64 {
	return (b.limit - b.held - int64(b.nopen)*streamHeap) / int64(max(1, users))
}

// pickZstd chooses a zstd stream's tier for frames of n bytes: the largest
// that holds them within its fair share and what is left, else the
// smallest that holds them while what is left holds it, else the last
// resort, or none. own is what the stream holds now, which it gives up.
// b.mu is held.
func (b *Budget) pickZstd(o Options, n int, own int64) (tier, bool) {
	fit, last := b.menu(Zstd, o, n)
	if b.limit == 0 {
		if len(fit) == 0 {
			return tier{}, false
		}
		return fit[0], true
	}
	left := b.limit - b.reserved + own
	for _, t := range fit {
		if t.heap <= min(left, b.share(b.users)) {
			return t, true
		}
	}
	if len(fit) > 0 && fit[len(fit)-1].heap <= left {
		return fit[len(fit)-1], true
	}
	if len(fit) > 0 {
		b.starved.Add(1)
	}
	if last.coding != "" && last.heap <= left {
		return last, true
	}
	return tier{}, false
}

// pickBrotli chooses a brotli stream's tier for frames of n bytes: the
// window that holds them at the stream's quality while the budget stays at
// most half full, else at quality 3 while what is left holds it, else the
// last resort. It gives up own, which the stream holds, and keeps it when
// nothing else fits. b.mu is held.
func (b *Budget) pickBrotli(o Options, n int, own tier) tier {
	fit, last := b.menu(Brotli, o, n)
	used := b.reserved - own.heap
	if len(fit) > 0 && used+fit[0].heap <= b.limit/2 {
		return fit[0]
	}
	for _, t := range slices.Backward(fit) {
		if used+t.heap <= b.limit {
			return t
		}
	}
	if len(fit) > 0 {
		b.starved.Add(1)
	}
	if last.coding != "" && used+last.heap <= b.limit {
		return last
	}
	return own
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
// for none, and reserves 32 KiB for it, and its tier: without a limit the
// largest; with one, gzip's, brotliFloor for a brotli stream until its
// first write picks its tier, and none for a zstd stream, which takes its
// tier at its first write. A stream that would not have room for the
// coding's smallest tier gets none. user reports whether the stream counts
// in the fair share, ok false that the budget refuses it. seen gets the
// count of streams that found no room, for shrink.
func (b *Budget) open(coding string, o Options, seen *int64) (t tier, user, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	defer func() { *seen = b.starved.Load() }()
	if coding != "" {
		fit, least := b.menu(coding, o, 0)
		if least.coding == "" && len(fit) > 0 {
			least = fit[len(fit)-1]
		}
		switch {
		case b.limit == 0 && len(fit) > 0:
			t = fit[0]
		case least.coding == "" || b.limit > 0 && least.heap > b.limit-b.reserved-streamHeap:
			b.starved.Add(1)
			if b.refuse {
				b.refused++
				return tier{}, false, false
			}
		case coding == Brotli:
			t = brotliFloor
		case coding == Gzip:
			t = least
		}
		if t.coding != "" || coding == Zstd {
			b.users++
			user = true
		}
	}
	if t.coding == "" && coding != Zstd {
		coding = ""
	}
	b.nopen++
	b.reserved += streamHeap
	b.hold(t, coding)
	return t, user, true
}

// first picks the tier of a brotli stream that holds t, brotliFloor, at its
// first write of n bytes.
func (b *Budget) first(t tier, o Options, n int) tier {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit == 0 {
		return t
	}
	nt := b.pickBrotli(o, n, t)
	b.drop(t, Brotli)
	b.hold(nt, Brotli)
	return nt
}

// wake reserves a tier for a zstd stream without one, whose largest write
// is n bytes, if one fits.
func (b *Budget) wake(o Options, n int, seen *int64) (tier, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.pickZstd(o, n, 0)
	*seen = b.starved.Load()
	if ok {
		b.drop(tier{}, Zstd)
		b.hold(t, Zstd)
	}
	return t, ok
}

// shrink moves a zstd stream that holds t, whose largest write is n bytes,
// down to the largest tier within its fair share that holds that frame,
// when a stream found no room since the stream's last tier (or the limit
// changed) and the share is smaller than t. When the share is below the
// smallest such tier, it moves to that tier while what is left holds it,
// else to the last resort, else to none, and from that tier, nowhere. A
// budget that refuses streams counts the one it turned away, which is not
// open. It reports the new tier, or false to keep t.
func (b *Budget) shrink(t tier, o Options, n int, seen *int64) (tier, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	*seen = b.starved.Load()
	users := b.users
	if b.refuse {
		users++
	}
	share := b.share(users)
	fit, last := b.menu(Zstd, o, n)
	if b.limit == 0 || t.heap <= share || len(fit) == 0 {
		return t, false
	}
	var nt tier
	for _, c := range fit {
		if c.heap <= share {
			nt = c
			break
		}
	}
	if nt.coding == "" {
		// The share is below the smallest tier that holds the frame: that
		// tier while what is left holds it, then the last resort, then none.
		if f := fit[len(fit)-1]; f.heap < t.heap {
			b.drop(t, Zstd)
			switch left := b.limit - b.reserved; {
			case f.heap <= left:
				nt = f
			case last.coding != "" && last.heap <= left:
				nt = last
			}
			b.hold(nt, Zstd)
			b.free()
			return nt, true
		}
		return t, false
	}
	if nt.heap >= t.heap {
		return t, false
	}
	// The move lowers what the streams hold, so it does not wait for what
	// is left: after the limit dropped, the streams above their share hold
	// more than the limit until they have all moved.
	b.drop(t, Zstd)
	b.hold(nt, Zstd)
	b.free()
	return nt, true
}

// lift moves a zstd stream that holds t, whose window is below its largest
// write of n bytes, to a tier that holds that frame, when one fits. It
// reports the new tier, or false to keep t.
func (b *Budget) lift(t tier, o Options, n int, seen *int64) (tier, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	*seen = b.freed.Load()
	nt, ok := b.pickZstd(o, n, t.heap)
	if !ok || nt == t || b.shortOf(nt, Zstd, o, n) {
		return t, false
	}
	b.drop(t, Zstd)
	b.hold(nt, Zstd)
	return nt, true
}

// sleep gives back the tier of a zstd stream that hibernates.
func (b *Budget) sleep(t tier) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.drop(t, Zstd)
	b.hold(tier{}, Zstd)
	b.free()
}

// close counts out a stream of the coding, "" for none, and gives back its
// tier, and takes its mover, if any, off the list.
func (b *Budget) close(t tier, coding string, user bool, m *mover) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.drop(t, coding)
	b.nopen--
	b.reserved -= streamHeap
	if user {
		b.users--
	}
	if m != nil && m.listed {
		m.listed = false
		b.movers = slices.DeleteFunc(b.movers, func(x *mover) bool { return x == m })
	}
	b.free()
}

// hold and drop count a stream's tier in and out. b.mu is held.
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

// mover is a stream that its budget may ask to open again, by closing ch:
// a brotli stream whose window is below what its frame needs, or one that
// went out uncompressed although its client takes brotli, while a new
// stream would get a window that holds the frame. Its fields are guarded
// by Budget.mu.
type mover struct {
	ch     chan struct{}
	frame  int  // the stream's largest write
	tier   tier // what it holds
	opt    Options
	listed bool // in Budget.movers
}

// update tells the budget what a mover holds and its largest write, and
// lists it among the movers while its window is below what that frame
// needs and some window would hold it.
func (b *Budget) update(m *mover, t tier, frame int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m.tier, m.frame = t, frame
	want := m.frame > 0 && b.shortOf(t, Brotli, m.opt, m.frame)
	switch {
	case want && !m.listed:
		m.listed = true
		b.movers = append(b.movers, m)
		b.promote()
	case !want && m.listed:
		m.listed = false
		b.movers = slices.DeleteFunc(b.movers, func(x *mover) bool { return x == m })
	}
}

// short reports whether a stream of the coding that holds t, whose
// largest write is n bytes, holds a window below what that frame needs,
// or none, while some window would hold it.
func (b *Budget) short(t tier, coding string, o Options, n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.shortOf(t, coding, o, n)
}

// shortOf is short with b.mu held.
func (b *Budget) shortOf(t tier, coding string, o Options, n int) bool {
	if b.limit == 0 {
		return false
	}
	fit, _ := b.menu(coding, o, n)
	return len(fit) > 0 && (t.coding == "" || t.window < fit[len(fit)-1].window)
}

// free notes that room freed up, for the zstd streams that wait for it
// and for the movers. b.mu is held.
func (b *Budget) free() {
	b.freed.Add(1)
	b.promote()
}

// promote asks the mover listed first that a new stream would serve with
// a window that holds its frame to open again: at most one every
// reopenEvery. When one could go later, a timer looks again then. b.mu is
// held.
func (b *Budget) promote() {
	if len(b.movers) == 0 || b.limit == 0 {
		return
	}
	now := time.Now()
	var next time.Time
	for i, m := range b.movers {
		fit, _ := b.menu(Brotli, m.opt, m.frame)
		if len(fit) == 0 || b.reserved-m.tier.heap+fit[len(fit)-1].heap > b.limit {
			continue
		}
		if at := b.asked.Add(b.reopenEvery); at.After(now) {
			next = at
			break
		}
		m.listed = false
		b.movers = slices.Delete(b.movers, i, i+1)
		close(m.ch)
		b.asked = now
		b.reopened++
		next = time.Time{}
		if len(b.movers) > 0 {
			next = now.Add(b.reopenEvery)
		}
		break
	}
	if next.IsZero() || b.timer != nil && !next.Before(b.timerAt) {
		return
	}
	if b.timer != nil {
		b.timer.Stop()
	}
	b.timerAt = next
	b.timer = time.AfterFunc(next.Sub(now), func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.timer = nil
		b.promote()
	})
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
