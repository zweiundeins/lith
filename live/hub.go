// Package live renders views from the database and streams them to
// pages: view = f(state).
//
// A View is a part of a page. Views with the same key are one view, which
// renders once for every stream that watches it. A commit marks views
// dirty through their topics (Changed, wired to the writer's OnCommit).
// The hub renders dirty views in ticks: its loop sleeps while nothing is
// dirty, starts a tick at once after a quiet spell, and runs at most one
// tick per Options.Tick. In a tick the dirty views go onto one queue that
// the render workers drain. Each worker holds a connection of the read
// pool and renders all its views of the tick in one read transaction.
// When every worker is done, the new frames go out.
//
// A view renders in one read transaction, so it agrees with itself. Views
// rendered by different workers can see different commits when one lands
// during the tick; that commit marks its views dirty, and the next tick
// brings them together. What must always agree belongs in one view.
//
// A view's topics are its Topics and those its last Render declared with
// Depends, so a view whose reads follow a row, such as a tab's route,
// follows them without a new key. Topics form a tree by their segments:
// a commit to src:A:7 marks the views of src:A and src as well, and one
// to src:A:* those of everything below src:A. A commit after a Render's
// snapshot, to a topic that Render declares for the first time, marks
// the view dirty once the hub takes the topic on.
//
// The hub numbers the commits Changed reports, 1, 2, 3 and on (Seq), and
// keeps the number of the last commit that touched each topic (Version):
// a cache of something a Render or a GET computes, such as a count, stays
// good while the version of its topics is not above the number it was
// computed at (Seq(ctx) in a Render).
//
// A stream keeps only the newest frame of each view it watches, so a slow
// client skips frames and holds up no one else. Views that change in the
// same tick reach a stream together, in the order it watches them. A
// stream to a client on a slow link (Spec.Slow) takes a view's frames
// about four times a second.
//
// A dirty view does not render while every stream that watches it still
// has a frame of it waiting: a stream that has not sent the last one has
// no use for the next. The hub defers the view instead
// (Stats.RendersDeferred), and the first stream that takes its waiting
// frame marks the view dirty again, so the next tick renders the state as
// it is then. A view stays deferred, and costs no render, while its
// streams' writes block, and a view that only slow streams watch renders
// about four times a second. A view that one stream keeps up with renders
// at every change, for all of its streams.
//
// The hub keeps a view's newest frame while two or more streams watch
// it, so that a stream that joins gets it at once. Of a view that one
// stream watches, such as a tab's own, it keeps only the frame's length
// and hash once the stream has the frame: a render that repeats it goes
// to no stream, and a stream that joins gets a frame rendered in the next
// tick. A frame that is not []byte has no hash, and the hub keeps it.
package live

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"hash/maphash"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zweiundeins/lith/sse"
)

// Options configure a Hub. The zero value of every field but Read
// selects its default.
type Options struct {
	// Read is the read pool. Each worker holds one of its connections for
	// the hub's lifetime, and everything else that reads (GET documents)
	// shares the rest: the pool needs Workers connections plus the reads
	// that run at once besides the hub. sqlite.Options.Readers' default,
	// GOMAXPROCS + 4 and at least 8, leaves at least four beside the
	// default Workers. Without Read, views render with a nil transaction.
	Read *sql.DB
	// Tick is the shortest time from one tick to the next (default 50ms).
	Tick time.Duration
	// Workers is the number of render workers (default GOMAXPROCS).
	Workers int
	// MaxStreams is how many streams may be open at once (default 2000).
	// Serve answers 503 to more. With a budget that refuses streams
	// (sse.BudgetOptions.Refuse), memory caps them, and MaxStreams is a
	// backstop.
	MaxStreams int
	// PendingWatch is how long Watch keeps a view for a stream ID that is
	// not open yet, for the stream that opens with it (default 10s,
	// negative: Watch keeps none). Views wait for at most MaxStreams IDs
	// at once.
	PendingWatch time.Duration
	// Heartbeat is how long a stream stays silent before it sends a
	// comment that keeps proxies from closing it (default 20s).
	Heartbeat time.Duration
	// HeartbeatSignal, if set, names a top-level signal, such as "_ping",
	// that the heartbeat patches instead of sending a comment, which the
	// page never sees: a watchdog on the page can then tell a stream that
	// went quiet without closing. The heartbeat then goes out every
	// Heartbeat, frames or not, and its number grows with every one.
	HeartbeatSignal string
	// Hibernate is how long a stream may send no frame before it hibernates
	// (sse.Stream.Hibernate): a zstd stream then gives its encoder back
	// until its next frame, which starts without the history of the ones
	// before. Heartbeats go out without waking it. Zero, the default, never
	// hibernates: every stream keeps its encoder while it is open.
	Hibernate time.Duration
	// RenderTimeout is how long a Render may take (default 2s): its ctx
	// ends then, and a Render still running is logged. Nothing stops a
	// Render that ignores its ctx. It holds up only its worker while the
	// others render the rest of the tick, but the tick, and with it every
	// frame, waits until it returns.
	RenderTimeout time.Duration
	// Log receives render errors and overrun reports (default
	// slog.Default()).
	Log *slog.Logger
	// OnTick, if set, gets the duration of every tick that rendered
	// views, as the overrun log measures it. It runs on the hub's loop
	// and must not block.
	OnTick func(time.Duration)
	// Versions is how many topics the hub keeps the version of (Version),
	// those changed last: at least Versions of them and at most twice as
	// many (default 32768; negative: none). A topic takes about 70 bytes
	// with a name of 16, so 4.3 MiB at most by default. A topic it no
	// longer keeps reads as changed at the commit that pushed it out, and
	// so does every topic while it keeps none: a cache then computes
	// again, and is never wrong. A bulk sync that touches more distinct
	// topics than Versions, one per item, pushes them out all along, and
	// a cache keyed on an item computes again about once per Versions new
	// topics until it ends. The topics above them, such as src:A above
	// src:A:7, are in PrefixVersions, which the sync does not push out: a
	// cache keyed on a space or a source computes again only when they
	// change.
	Versions int
	// PrefixVersions is how many prefixes the hub keeps the version of,
	// the topics above those a commit names, such as board for board:42
	// (default 32768; negative: none). It bounds them as Versions bounds
	// the topics the commits name, and a prefix it no longer keeps reads
	// as changed in the same way. A topic that a commit names while the
	// hub keeps it as a prefix stays among the prefixes.
	PrefixVersions int
	// SSE configures the streams. Its Compress semaphore defaults to half
	// the CPUs, so a burst of frames leaves cores for everything else. Its
	// Budget defaults to sse.NewBudget(sse.BudgetOptions{}): half the
	// process's memory limit for the streams, their encoders and the frames
	// the hub keeps, which it books (sse.Budget.Hold); each stream gets a
	// window that holds its frames, up to 2^19, and goes out uncompressed
	// when not even the smallest tier fits; without a memory limit it only
	// counts. Pass a Budget of your own for another limit, or with Refuse,
	// which Serve answers with 503 when the budget has no room for a
	// stream: for a window that holds its first frames, or for the 32 KiB
	// of one that compresses nothing; or one with Limit -1 to turn it off.
	SSE sse.Options
}

// View is a part of a page that the hub renders and streams.
type View struct {
	// Key names the view. Views with the same key are one view: while it
	// is watched, the definition it was first given is the one used.
	Key string
	// Topics mark the view dirty when a commit touches one of them, and so
	// do the topics its last Render declared with Depends. A topic is a
	// path of segments joined by ':', such as board:42, and a commit to a
	// topic below one touches it too: a view on board renders for a commit
	// to board:42 (Hub.Changed). A segment is never empty and holds no '*';
	// Serve and Watch panic on a topic that breaks this.
	Topics []string
	// Render renders the view in the worker's read transaction. Its ctx
	// comes from the one Run got, with its values (a logger, the app's
	// services), ends after Options.RenderTimeout and carries the
	// transaction (sqlite.TxFrom): a sqlite.DB.View called with ctx runs
	// in it, so code that takes a ctx reads the render's snapshot.
	// sqlite.QuerierFrom(ctx) runs queries in it on statements the worker
	// prepared once on its connection, and Seq(ctx) is the number of the
	// last commit the snapshot is sure to hold. Its frame goes to every
	// stream that watches the view and must not change afterwards. When
	// Render fails, the error is logged and the streams send nothing; so
	// they do for a nil frame, and for a []byte frame equal to the one
	// before (its bytes while the hub keeps them, else its length and
	// hash). The hub keeps the newest frame while two or more streams
	// watch the view, or one does and the frame is not []byte, and books
	// it in the streams' budget once: a []byte frame by its length, another
	// by its Len() int method, if it has one, each with 8 KiB besides.
	Render func(ctx context.Context, tx *sql.Tx) (any, error)
	// Every renders the view again after this long without changes
	// (clocks, live stats).
	Every time.Duration
	// Presence marks the view dirty whenever a stream starts or stops
	// watching it (see Count).
	Presence bool
}

// Stats count the hub's work since it started.
type Stats struct {
	Ticks         int64 // ticks that rendered views
	Renders       int64
	FramesSent    int64 // frames of views written to streams
	FramesDropped int64 // frames a newer one replaced before their stream sent them
	// RendersDeferred counts the times a dirty view was not rendered
	// because every stream that watches it still had a frame of it waiting.
	RendersDeferred int64
	Overruns        int64 // ticks that took longer than Options.Tick
	// Behind counts the reports, through Hub.Behind, that a stream's page
	// fell behind it (newest-frame.js's newest-frame-behind), and
	// BehindStreams the streams that reported at least once.
	Behind        int64
	BehindStreams int64
	Acks          int64 // acknowledgements of streams with Spec.Window (Hub.Ack)
	Paces         int64 // streams that Hub.Pace started to pace
	Releases      int64 // paced streams whose page kept up for Spec.Release
	Streams       int
	Views         int
	ReadTxs       int64 // read transactions the render workers began
	// RendersByKey counts the renders of each view watched now, by its
	// key, since it was first watched.
	RendersByKey map[string]int64
}

// Hub renders views and streams them. Create it with New and start it
// with Run.
type Hub struct {
	read          *sql.DB
	tick          time.Duration
	workers       int
	maxStreams    int
	pendingFor    time.Duration
	heartbeat     time.Duration
	hibernate     time.Duration
	ping          []byte       // the heartbeat signal's patch up to its number, nil for none
	pings         atomic.Int64 // the heartbeat signal's last number
	seed          maphash.Seed // hashes []byte frames for the unchanged-frame check
	renderTimeout time.Duration
	log           *slog.Logger
	onTick        func(time.Duration)
	sse           sse.Options
	sampleFor     time.Duration // how long tick durations are sampled after an overrun

	done    context.Context // ends when the hub stops
	stop    context.CancelFunc
	running atomic.Bool
	wake    chan struct{}
	seq     atomic.Uint64 // numbers the streams
	commits atomic.Uint64 // the last commit Changed numbered (Seq); written with mu and vmu held

	// The versions of the topics changed last (Options.Versions), and
	// those of their prefixes (Options.PrefixVersions).
	vmu      sync.RWMutex
	leaves   versions[uint64]
	prefixes versions[*stamp]

	mu      sync.Mutex
	views   map[string]*view
	topics  map[string]*node // the tree of the topics views listen to, by name
	every   map[*view]struct{}
	dirty   []*view
	byID    map[string]*stream
	streams int
	pending map[string][]pendingView // by stream ID, in the order of Watch
	sweepAt time.Time                // when pending is next swept
	asked   map[string]time.Time     // stream IDs asked to open again for a better tier, and when
	next    chan struct{}            // closed once the next tick that renders views has published its frames

	ticks, renders, sent, dropped, deferred, overruns, readTxs, acks, paces, releases atomic.Int64
	behind, behindStreams                                                             atomic.Int64

	samplesMu sync.Mutex
	samples   []time.Duration // nil while no overrun is being sampled
	report    *time.Timer
}

type view struct {
	key      string
	topics   []string
	deps     []string // the topics its last Render declared, sorted; set in publish, with Hub.mu held
	render   func(ctx context.Context, tx *sql.Tx) (any, error)
	every    time.Duration
	presence bool

	renders atomic.Int64
	// deferred: dirty, but left unrendered while every watch had a frame of
	// it waiting; the pump that takes one marks the view dirty again.
	deferred atomic.Bool

	// Guarded by Hub.mu.
	frame     *frame // the newest frame while two or more streams watch the view, else nil
	size      int    // the newest frame's length, -1 when it is not []byte or there is none
	sum       uint64 // its hash, for the unchanged-frame check once the hub drops its bytes
	booked    int64  // what the hub booked for frame in the budget
	watches   []*watch
	dirty     bool
	dead      bool      // no longer watched
	rendering bool      // in the tick under way
	joined    bool      // a stream started watching it during that tick, without a frame
	owed      bool      // a watch lacks the newest frame (watch.owed)
	due       time.Time // when Every renders it again
}

type frame struct {
	value any
}

// watch is one stream watching one view.
type watch struct {
	s    *stream
	v    *view
	i    int       // index in v.watches, guarded by Hub.mu
	owed bool      // joined while the hub kept no frame, until a render brings one; guarded by Hub.mu
	next *frame    // the newest frame not sent yet, guarded by s.mu
	gone bool      // unwatched, guarded by s.mu
	sent time.Time // when the stream last took a frame of the view, with Spec.Slow; guarded by s.mu
}

type stream struct {
	seq       uint64 // order of arrival
	id        string
	write     func(*sse.Stream, string, any) error
	writeTick func(*sse.Stream, []Frame) error
	cancel    context.CancelFunc
	wake      chan struct{}
	upgrade   time.Duration // when it listens for a better tier (sse.Stream.Upgrade); negative: never
	every     time.Duration // the least time between two frames of a view (Spec.Slow), 0 for none
	ack       func(token string, n int64) []byte
	release   time.Duration // Spec.Release
	slack     time.Duration // Spec.ReleaseSlack

	mu sync.Mutex
	// The views the stream watches, in the order it started watching
	// them. Changed with both Hub.mu and mu held, read with either.
	watches []*watch
	// With Spec.Ack: the writes that may wait for the page's
	// acknowledgement (Spec.Window, Hub.Pace), 0 for no pacing; the
	// paced writes of frames, the newest acknowledged, when the stream
	// last heard from the page or started to wait for it, and whether the
	// window was full when the pump last looked. late is when an
	// acknowledgement last came after a frame was ready, or when pacing
	// started, and releases counts the ends of pacing (Spec.Release).
	window, written, acked int64
	heard, late            time.Time
	full                   bool
	releases               int
	// With Spec.ReleaseSlack: when the newest paced writes went out, by n,
	// and the shortest round trip the page reported (Hub.RoundTrip).
	wroteAt [16]time.Time
	rtt     time.Duration
	// behindAt is when Hub.Behind last counted a report of the page.
	behindAt time.Time
}

// New returns a hub; Run starts it.
func New(o Options) *Hub {
	if o.Tick <= 0 {
		o.Tick = 50 * time.Millisecond
	}
	if o.Workers <= 0 {
		o.Workers = runtime.GOMAXPROCS(0)
	}
	if o.MaxStreams <= 0 {
		o.MaxStreams = 2000
	}
	if o.PendingWatch == 0 {
		o.PendingWatch = 10 * time.Second
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = 20 * time.Second
	}
	if o.RenderTimeout <= 0 {
		o.RenderTimeout = 2 * time.Second
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Versions == 0 {
		o.Versions = 1 << 15
	}
	if o.PrefixVersions == 0 {
		o.PrefixVersions = 1 << 15
	}
	if o.SSE.Budget == nil {
		o.SSE.Budget = sse.NewBudget(sse.BudgetOptions{})
	}
	if o.SSE.Compress == nil {
		o.SSE.Compress = make(chan struct{}, max(1, runtime.GOMAXPROCS(0)/2))
	}
	var ping []byte
	if o.HeartbeatSignal != "" {
		name, _ := json.Marshal(o.HeartbeatSignal)
		ping = append(append([]byte("{"), name...), ':')
	}
	done, stop := context.WithCancel(context.Background())
	h := &Hub{
		read:          o.Read,
		tick:          o.Tick,
		workers:       o.Workers,
		maxStreams:    o.MaxStreams,
		pendingFor:    o.PendingWatch,
		heartbeat:     o.Heartbeat,
		hibernate:     max(0, o.Hibernate),
		ping:          ping,
		seed:          maphash.MakeSeed(),
		renderTimeout: o.RenderTimeout,
		log:           o.Log,
		onTick:        o.OnTick,
		sse:           o.SSE,
		sampleFor:     20 * time.Second,
		done:          done,
		stop:          stop,
		wake:          make(chan struct{}, 1),
		leaves:        newVersions[uint64](o.Versions),
		prefixes:      newVersions[*stamp](o.PrefixVersions),
		views:         map[string]*view{},
		topics:        map[string]*node{},
		every:         map[*view]struct{}{},
		byID:          map[string]*stream{},
		pending:       map[string][]pendingView{},
		asked:         map[string]time.Time{},
		next:          make(chan struct{}),
	}
	// From the clock, so a page that reconnects to a new process still
	// sees the number grow.
	h.pings.Store(time.Now().UnixMilli())
	return h
}

// Changed marks dirty every view with one of the topics, or with a
// topic above one: a commit to src:A:7 touches src:A and src as well. A
// topic that ends in :*, such as src:A:*, touches everything at or below
// src:A, and src above it. A topic of * alone touches everything, as
// MarkAll. A topic or one above it that the topics name twice counts
// once. Changed does not check the topics, as Watch does: it runs for
// every commit.
//
// It is meant for the writer's OnCommit and never waits for a render.
// Each call is a commit: it takes the next number (Seq), which becomes
// the version of the topics it touches. Call it after the change is in
// the database, as OnCommit is. An app that commits through its own
// store calls it after each commit is visible to readers, never inside
// the transaction: a render that begins after Changed counts the commit
// as part of its snapshot.
func (h *Hub) Changed(topics []string) {
	h.mu.Lock()
	h.vmu.Lock()
	n := h.commits.Load() + 1
	all := false
	for _, t := range topics {
		if t == "*" {
			all = true
		} else {
			h.touch(t, n)
		}
	}
	if all {
		h.leaves.forget(n)
		h.prefixes.forget(n)
	}
	h.commits.Store(n)
	h.vmu.Unlock()
	marked := false
	if all {
		for _, v := range h.views {
			marked = h.mark(v) || marked
		}
	} else {
		for _, t := range topics {
			if below, ok := strings.CutSuffix(t, ":*"); ok {
				marked = h.markBelow(below, n) || marked
			} else {
				marked = h.markUp(t, n) || marked
			}
		}
	}
	h.mu.Unlock()
	if marked {
		h.poke()
	}
}

var everything = []string{"*"}

// MarkAll marks every view dirty, as after a change nothing tracks
// (a migration, new templates). It counts as a commit that touched every
// topic: Version then reports it for each. It is Changed with *.
func (h *Hub) MarkAll() { h.Changed(everything) }

// Seq is the number of the last commit: Changed and MarkAll number them
// from 1, and 0 is the hub's start. The numbers start again with every
// hub, so keep nothing keyed by them beyond the process.
//
// A cache that a GET fills reads Seq before its read transaction begins:
// every commit up to that number is in the snapshot, and a later one may
// be too. The value it computes is good as of that number, and stays good
// while Version of its topics is not above it. Read after the snapshot,
// the number could claim a commit the snapshot does not hold. A Render
// takes the number from Seq(ctx).
func (h *Hub) Seq() uint64 { return h.commits.Load() }

// Version is the number of the last commit that touched one of the
// topics (Seq), 0 when none did since the hub started. A commit touches
// a topic as Changed says: Version of src:A moves with every commit to
// src:A:7, and Version of src:A:7 with one to src:A:*. A topic with :*
// reads as the topic without it, and * as Seq. A topic the hub no longer
// keeps (Options.Versions, Options.PrefixVersions) reads as changed at
// the commit that pushed it, or a topic above it, out of the hub, or at
// the last MarkAll; so do no topics.
func (h *Hub) Version(topics ...string) uint64 {
	h.vmu.RLock()
	defer h.vmu.RUnlock()
	if len(topics) == 0 {
		return max(h.leaves.floor, h.prefixes.floor)
	}
	var n uint64
	for _, t := range topics {
		if t == "*" {
			return h.commits.Load()
		}
		t, _ = strings.CutSuffix(t, ":*")
		n = max(n, h.version(t))
	}
	return n
}

// Count is the number of streams watching the view.
func (h *Hub) Count(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if v := h.views[key]; v != nil {
		return len(v.watches)
	}
	return 0
}

// Watchers are the IDs of the streams watching the view, each once, in the
// order the streams opened; streams without an ID are left out. A view
// with Presence that calls it in its Render lists who is there.
func (h *Hub) Watchers(key string) []string {
	h.mu.Lock()
	v := h.views[key]
	if v == nil {
		h.mu.Unlock()
		return nil
	}
	ss := make([]*stream, 0, len(v.watches))
	for _, w := range v.watches {
		if w.s.id != "" {
			ss = append(ss, w.s)
		}
	}
	h.mu.Unlock()
	slices.SortFunc(ss, func(a, b *stream) int { return cmp.Compare(a.seq, b.seq) })
	ids := make([]string, 0, len(ss))
	seen := make(map[string]bool, len(ss))
	for _, s := range ss {
		// A stream that replaces one with its ID watches beside it until
		// the old one has ended.
		if !seen[s.id] {
			seen[s.id] = true
			ids = append(ids, s.id)
		}
	}
	return ids
}

// Watch makes the open stream with this ID watch v as well: it gets v's
// newest frame at once when the hub keeps it (View.Render), else a frame
// rendered in the next tick. It reports whether such a stream is open. If
// none is, as when a page's command arrives before its stream, a stream
// that opens with the ID within Options.PendingWatch watches v from the
// start, after the views of its Spec.
func (h *Hub) Watch(streamID string, v View) bool {
	check(v)
	if streamID == "" {
		return false
	}
	h.mu.Lock()
	s := h.byID[streamID]
	if s == nil {
		h.pend(streamID, v, time.Now())
		h.mu.Unlock()
		return false
	}
	marked := h.watch(s, v)
	h.mu.Unlock()
	if marked {
		h.poke()
	}
	return true
}

// Unwatch stops the stream with this ID from watching the view: after it
// returns, the stream sends no frame of the view, unless it is writing
// one already. It reports whether the stream watched the view, or the
// view waited for the stream to open.
func (h *Hub) Unwatch(streamID, key string) bool {
	h.mu.Lock()
	s := h.byID[streamID]
	if streamID == "" || s == nil {
		ps := h.pending[streamID]
		i := slices.IndexFunc(ps, func(p pendingView) bool { return p.def.Key == key })
		if i >= 0 {
			if ps = slices.Delete(ps, i, i+1); len(ps) == 0 {
				delete(h.pending, streamID)
			} else {
				h.pending[streamID] = ps
			}
		}
		h.mu.Unlock()
		return i >= 0
	}
	i := slices.IndexFunc(s.watches, func(w *watch) bool { return w.v.key == key })
	if i < 0 {
		h.mu.Unlock()
		return false
	}
	w := s.watches[i]
	marked := h.drop(w)
	s.mu.Lock()
	w.gone, w.next = true, nil
	s.watches = slices.Delete(s.watches, i, i+1)
	s.mu.Unlock()
	h.mu.Unlock()
	if marked {
		h.poke()
	}
	return true
}

// Done is closed once the hub stops: when Run's ctx ends, or Run returns.
// Work that only serves the hub's views ends with it, such as a clock
// that changes a view while someone watches it (Count).
func (h *Hub) Done() <-chan struct{} { return h.done.Done() }

// Budget is the budget the hub's streams share: Options.SSE.Budget, or the
// one New made. Its Stats tell what the streams hold, the frames the hub
// keeps among it, and SetLimit moves its limit while the hub runs.
func (h *Hub) Budget() *sse.Budget { return h.sse.Budget }

// Stats reads the counters.
func (h *Hub) Stats() Stats {
	h.mu.Lock()
	streams, views := h.streams, len(h.views)
	byKey := make(map[string]int64, len(h.views))
	for k, v := range h.views {
		byKey[k] = v.renders.Load()
	}
	h.mu.Unlock()
	return Stats{
		Ticks:           h.ticks.Load(),
		Renders:         h.renders.Load(),
		FramesSent:      h.sent.Load(),
		FramesDropped:   h.dropped.Load(),
		RendersDeferred: h.deferred.Load(),
		Overruns:        h.overruns.Load(),
		Behind:          h.behind.Load(),
		BehindStreams:   h.behindStreams.Load(),
		Acks:            h.acks.Load(),
		Paces:           h.paces.Load(),
		Releases:        h.releases.Load(),
		Streams:         streams,
		Views:           views,
		ReadTxs:         h.readTxs.Load(),
		RendersByKey:    byKey,
	}
}

func check(v View) {
	if v.Key == "" || v.Render == nil {
		panic("live: a View needs a Key and a Render")
	}
	for _, t := range v.Topics {
		checkTopic(t)
	}
}

func (h *Hub) poke() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (s *stream) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// pendingView is a view that waits for its stream to open.
type pendingView struct {
	def   View
	until time.Time
}

// pend keeps def for the stream that opens with id before h.pendingFor
// has passed. Hub.mu is held.
func (h *Hub) pend(id string, def View, now time.Time) {
	if h.pendingFor < 0 {
		return
	}
	if !now.Before(h.sweepAt) {
		for k, ps := range h.pending {
			ps = slices.DeleteFunc(ps, func(p pendingView) bool { return !now.Before(p.until) })
			if len(ps) == 0 {
				delete(h.pending, k)
			} else {
				h.pending[k] = ps
			}
		}
		h.sweepAt = now.Add(h.pendingFor)
	}
	ps := h.pending[id]
	if ps == nil && len(h.pending) >= h.maxStreams {
		return
	}
	p := pendingView{def: def, until: now.Add(h.pendingFor)}
	if i := slices.IndexFunc(ps, func(p pendingView) bool { return p.def.Key == def.Key }); i >= 0 {
		ps[i] = p
		return
	}
	h.pending[id] = append(ps, p)
}

// mark puts v on the dirty list and reports whether it was not on it
// yet. Hub.mu is held.
func (h *Hub) mark(v *view) bool {
	if v.dirty || v.dead {
		return false
	}
	v.dirty = true
	h.dirty = append(h.dirty, v)
	return true
}

// watch makes s watch the view def describes, creating the view if
// nobody watches it yet. It reports whether it marked a view dirty.
// Hub.mu is held.
func (h *Hub) watch(s *stream, def View) bool {
	if slices.ContainsFunc(s.watches, func(w *watch) bool { return w.v.key == def.Key }) {
		return false
	}
	marked := false
	v := h.views[def.Key]
	if v == nil {
		v = &view{key: def.Key, topics: slices.Clone(def.Topics), render: def.Render, every: def.Every, presence: def.Presence, size: -1}
		h.views[v.key] = v
		for _, t := range v.topics {
			h.subscribe(v, t)
		}
		if v.every > 0 {
			h.every[v] = struct{}{}
		}
		marked = h.mark(v)
	} else if v.presence {
		marked = h.mark(v)
	} else if v.frame == nil {
		// A render under way publishes to every watch, this one too.
		if v.rendering {
			v.joined = true
		} else {
			marked = h.mark(v)
		}
	}
	w := &watch{s: s, v: v, i: len(v.watches)}
	v.watches = append(v.watches, w)
	f := v.frame
	if f == nil {
		w.owed, v.owed = true, true
	}
	s.mu.Lock()
	s.watches = append(s.watches, w)
	w.next = f
	s.mu.Unlock()
	if f != nil {
		s.poke()
	}
	return marked
}

// drop takes w off its view, and the view off the hub when nobody
// watches it any more. The caller takes w off its stream. It reports
// whether it marked a view dirty. Hub.mu is held.
func (h *Hub) drop(w *watch) bool {
	v := w.v
	last := v.watches[len(v.watches)-1]
	v.watches[w.i] = last
	last.i = w.i
	v.watches[len(v.watches)-1] = nil
	v.watches = v.watches[:len(v.watches)-1]
	if len(v.watches) > 0 {
		// One stream left has the frame, which v may drop.
		h.sse.Budget.Hold(h.keep(v, nil))
		return v.presence && h.mark(v)
	}
	v.dead = true
	h.sse.Budget.Hold(-v.booked)
	v.frame, v.booked = nil, 0
	delete(h.views, v.key)
	for _, t := range v.topics {
		h.unsubscribe(v, t)
	}
	for _, t := range v.deps {
		h.unsubscribe(v, t)
	}
	delete(h.every, v)
	return false
}

// redepend makes deps, sorted and without duplicates, the topics v's
// Render declared, in place of those before, and reports whether a commit
// after seq touched one v takes on: the Render's snapshot, which holds
// the commits up to seq, may lack it, and no Changed marked v for it. A
// topic among v's Topics stays. Hub.mu is held.
func (h *Hub) redepend(v *view, deps []string, seq uint64) bool {
	stale := false
	h.vmu.RLock()
	old := v.deps
	i, j := 0, 0
	for i < len(old) || j < len(deps) {
		switch {
		case j == len(deps) || i < len(old) && old[i] < deps[j]:
			if !slices.Contains(v.topics, old[i]) {
				h.unsubscribe(v, old[i])
			}
			i++
		case i == len(old) || deps[j] < old[i]:
			if t := deps[j]; !slices.Contains(v.topics, t) {
				h.subscribe(v, t)
				stale = stale || h.version(t) > seq
			}
			j++
		default:
			i++
			j++
		}
	}
	h.vmu.RUnlock()
	v.deps = deps
	return stale
}

// keep keeps nf as v's frame, or the frame it keeps already, which nf
// repeats: while two or more streams watch v, or one does and the frame
// is not []byte, which has no hash and would go to that stream again if a
// join rendered it. Else the stream that watches v has the frame in its
// pending slot or wrote it, and v keeps none. It returns the change in
// what the hub books for v. Hub.mu is held.
func (h *Hub) keep(v *view, nf *frame) int64 {
	switch {
	case len(v.watches) == 0, len(v.watches) == 1 && v.size >= 0:
		v.frame = nil
	case v.frame == nil:
		v.frame = nf
	}
	var n int64
	if v.frame != nil {
		n = frameHeap(v.frame.value)
	}
	d := n - v.booked
	v.booked = n
	return d
}

// frameSlack is what a kept frame holds besides its bytes: the heap's
// pages of 8 KiB and the view's own state (bench/budget/viewcost.csv).
const frameSlack = 8 << 10

// frameHeap is what the hub books for a view's frame: its length and
// frameSlack, or nothing when the frame has no length.
func frameHeap(f any) int64 {
	n := frameLen(f)
	if n < 0 {
		return 0
	}
	return int64(n) + frameSlack
}

// frameLen is the length of a frame: a []byte's, else what its Len() int
// method reports, else -1.
func frameLen(f any) int {
	switch f := f.(type) {
	case []byte:
		return len(f)
	case interface{ Len() int }:
		return f.Len()
	}
	return -1
}

// unchanged reports whether f, whose hash is sum when it is []byte,
// repeats v's newest frame: the same bytes while v keeps them, else the
// same length and hash. Hub.mu is held.
func (v *view) unchanged(f any, sum uint64) bool {
	b, ok := f.([]byte)
	if !ok || v.size != len(b) || v.sum != sum {
		return false
	}
	if v.frame == nil {
		return true
	}
	a, ok := v.frame.value.([]byte)
	return ok && bytes.Equal(a, b)
}
