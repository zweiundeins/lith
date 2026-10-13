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
// A stream keeps only the newest frame of each view it watches, so a slow
// client skips frames and holds up no one else. Views that change in the
// same tick reach a stream together, in the order it watches them.
package live

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"runtime"
	"slices"
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
	// Serve answers 503 to more.
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
	// SSE configures the streams. Its Compress semaphore defaults to half
	// the CPUs, so a burst of frames leaves cores for everything else. Its
	// Budget defaults to sse.NewBudget(sse.BudgetOptions{}): half the
	// process's memory limit for the encoders, windows from 2^19 down to
	// 2^18, uncompressed streams when even 2^18 does not fit; without a
	// memory limit it only counts. Pass a Budget of your own for another
	// limit or Refuse, or one with Limit -1 to turn it off.
	SSE sse.Options
}

// View is a part of a page that the hub renders and streams.
type View struct {
	// Key names the view. Views with the same key are one view: while it
	// is watched, the definition it was first given is the one used.
	Key string
	// Topics mark the view dirty when a commit touches one of them.
	Topics []string
	// Render renders the view in the worker's read transaction. Its ctx
	// comes from the one Run got, with its values (a logger, the app's
	// services), ends after Options.RenderTimeout and carries the
	// transaction (sqlite.TxFrom): a sqlite.DB.View called with ctx runs
	// in it, so code that takes a ctx reads the render's snapshot.
	// sqlite.QuerierFrom(ctx) runs queries in it on statements the worker
	// prepared once on its connection. Its frame goes to every stream that
	// watches the view and must not change afterwards. When Render fails,
	// the error is logged and the previous frame stays; a nil frame also
	// keeps the previous one, and so does a []byte frame equal to it.
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
	Overruns      int64 // ticks that took longer than Options.Tick
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

	mu      sync.Mutex
	views   map[string]*view
	topics  map[string]map[*view]struct{}
	every   map[*view]struct{}
	dirty   []*view
	byID    map[string]*stream
	streams int
	pending map[string][]pendingView // by stream ID, in the order of Watch
	sweepAt time.Time                // when pending is next swept

	ticks, renders, sent, dropped, overruns, readTxs atomic.Int64

	samplesMu sync.Mutex
	samples   []time.Duration // nil while no overrun is being sampled
	report    *time.Timer
}

type view struct {
	key      string
	topics   []string
	render   func(ctx context.Context, tx *sql.Tx) (any, error)
	every    time.Duration
	presence bool

	frame   atomic.Pointer[frame]
	renders atomic.Int64

	// Guarded by Hub.mu.
	watches   []*watch
	dirty     bool
	dead      bool      // no longer watched
	rendering bool      // in the tick under way
	joined    bool      // a stream started watching it during that tick, before its first frame
	due       time.Time // when Every renders it again
}

type frame struct {
	value any
}

// watch is one stream watching one view.
type watch struct {
	s    *stream
	v    *view
	i    int    // index in v.watches, guarded by Hub.mu
	next *frame // the newest frame not sent yet, guarded by s.mu
	gone bool   // unwatched, guarded by s.mu
}

type stream struct {
	seq       uint64 // order of arrival
	id        string
	write     func(*sse.Stream, string, any) error
	writeTick func(*sse.Stream, []Frame) error
	cancel    context.CancelFunc
	wake      chan struct{}

	mu sync.Mutex
	// The views the stream watches, in the order it started watching
	// them. Changed with both Hub.mu and mu held, read with either.
	watches []*watch
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
		renderTimeout: o.RenderTimeout,
		log:           o.Log,
		onTick:        o.OnTick,
		sse:           o.SSE,
		sampleFor:     20 * time.Second,
		done:          done,
		stop:          stop,
		wake:          make(chan struct{}, 1),
		views:         map[string]*view{},
		topics:        map[string]map[*view]struct{}{},
		every:         map[*view]struct{}{},
		byID:          map[string]*stream{},
		pending:       map[string][]pendingView{},
	}
	// From the clock, so a page that reconnects to a new process still
	// sees the number grow.
	h.pings.Store(time.Now().UnixMilli())
	return h
}

// Changed marks dirty every view with one of the topics. It is meant for
// the writer's OnCommit and never waits for a render.
func (h *Hub) Changed(topics []string) {
	h.mu.Lock()
	marked := false
	for _, t := range topics {
		for v := range h.topics[t] {
			marked = h.mark(v) || marked
		}
	}
	h.mu.Unlock()
	if marked {
		h.poke()
	}
}

// MarkAll marks every view dirty, as after a change nothing tracks
// (a migration, new templates).
func (h *Hub) MarkAll() {
	h.mu.Lock()
	marked := false
	for _, v := range h.views {
		marked = h.mark(v) || marked
	}
	h.mu.Unlock()
	if marked {
		h.poke()
	}
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
// current frame at once, or its first one. It reports whether such a
// stream is open. If none is, as when a page's command arrives before its
// stream, a stream that opens with the ID within Options.PendingWatch
// watches v from the start, after the views of its Spec.
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
// one New made. Its Stats tell what the encoders hold, and SetLimit moves
// its limit while the hub runs.
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
		Ticks:         h.ticks.Load(),
		Renders:       h.renders.Load(),
		FramesSent:    h.sent.Load(),
		FramesDropped: h.dropped.Load(),
		Overruns:      h.overruns.Load(),
		Streams:       streams,
		Views:         views,
		ReadTxs:       h.readTxs.Load(),
		RendersByKey:  byKey,
	}
}

func check(v View) {
	if v.Key == "" || v.Render == nil {
		panic("live: a View needs a Key and a Render")
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
		v = &view{key: def.Key, topics: slices.Clone(def.Topics), render: def.Render, every: def.Every, presence: def.Presence}
		h.views[v.key] = v
		for _, t := range v.topics {
			m := h.topics[t]
			if m == nil {
				m = map[*view]struct{}{}
				h.topics[t] = m
			}
			m[v] = struct{}{}
		}
		if v.every > 0 {
			h.every[v] = struct{}{}
		}
		marked = h.mark(v)
	} else if v.presence {
		marked = h.mark(v)
	} else if v.frame.Load() == nil {
		// A render under way publishes to every watch, this one too.
		if v.rendering {
			v.joined = true
		} else {
			marked = h.mark(v)
		}
	}
	w := &watch{s: s, v: v, i: len(v.watches)}
	v.watches = append(v.watches, w)
	f := v.frame.Load()
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
		return v.presence && h.mark(v)
	}
	v.dead = true
	delete(h.views, v.key)
	for _, t := range v.topics {
		delete(h.topics[t], v)
		if len(h.topics[t]) == 0 {
			delete(h.topics, t)
		}
	}
	delete(h.every, v)
	return false
}

// same reports whether a frame repeats the published one.
func same(old *frame, f any) bool {
	if old == nil {
		return false
	}
	a, ok := old.value.([]byte)
	b, ok2 := f.([]byte)
	return ok && ok2 && bytes.Equal(a, b)
}
