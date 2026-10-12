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
	"context"
	"database/sql"
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
	// the hub's lifetime, so the pool needs more connections than there
	// are workers. Without it, views render with a nil transaction.
	Read *sql.DB
	// Tick is the shortest time from one tick to the next (default 50ms).
	Tick time.Duration
	// Workers is the number of render workers (default GOMAXPROCS).
	Workers int
	// MaxStreams is how many streams may be open at once (default 2000).
	// Serve answers 503 to more.
	MaxStreams int
	// Heartbeat is how long a stream stays silent before it sends a
	// comment that keeps proxies from closing it (default 20s).
	Heartbeat time.Duration
	// Log receives render errors and overrun reports (default
	// slog.Default()).
	Log *slog.Logger
	// SSE configures the streams. Its Compress semaphore defaults to half
	// the CPUs, so a burst of frames leaves cores for everything else.
	SSE sse.Options
}

// View is a part of a page that the hub renders and streams.
type View struct {
	// Key names the view. Views with the same key are one view: while it
	// is watched, the definition it was first given is the one used.
	Key string
	// Topics mark the view dirty when a commit touches one of them.
	Topics []string
	// Render renders the view in the worker's read transaction. Its frame
	// goes to every stream that watches the view and must not change
	// afterwards. When Render fails, the error is logged and the previous
	// frame stays; a nil frame also keeps the previous one, and so does a
	// []byte frame equal to it.
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
}

// Hub renders views and streams them. Create it with New and start it
// with Run.
type Hub struct {
	read       *sql.DB
	tick       time.Duration
	workers    int
	maxStreams int
	heartbeat  time.Duration
	log        *slog.Logger
	sse        sse.Options
	sampleFor  time.Duration // how long tick durations are sampled after an overrun

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

	ticks, renders, sent, dropped, overruns atomic.Int64

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

	frame atomic.Pointer[frame]

	// Guarded by Hub.mu.
	watches []*watch
	dirty   bool
	dead    bool      // no longer watched
	due     time.Time // when Every renders it again
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
	seq    uint64 // order of arrival
	id     string
	write  func(*sse.Stream, string, any) error
	cancel context.CancelFunc
	wake   chan struct{}

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
	if o.Heartbeat <= 0 {
		o.Heartbeat = 20 * time.Second
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.SSE.Compress == nil {
		o.SSE.Compress = make(chan struct{}, max(1, runtime.GOMAXPROCS(0)/2))
	}
	done, stop := context.WithCancel(context.Background())
	return &Hub{
		read:       o.Read,
		tick:       o.Tick,
		workers:    o.Workers,
		maxStreams: o.MaxStreams,
		heartbeat:  o.Heartbeat,
		log:        o.Log,
		sse:        o.SSE,
		sampleFor:  20 * time.Second,
		done:       done,
		stop:       stop,
		wake:       make(chan struct{}, 1),
		views:      map[string]*view{},
		topics:     map[string]map[*view]struct{}{},
		every:      map[*view]struct{}{},
		byID:       map[string]*stream{},
	}
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

// Watch makes the open stream with this ID watch v as well: it gets v's
// current frame at once, or its first one. It reports whether there is
// such a stream.
func (h *Hub) Watch(streamID string, v View) bool {
	check(v)
	h.mu.Lock()
	s := h.byID[streamID]
	if streamID == "" || s == nil {
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
// one already. It reports whether the stream watched the view.
func (h *Hub) Unwatch(streamID, key string) bool {
	h.mu.Lock()
	s := h.byID[streamID]
	if streamID == "" || s == nil {
		h.mu.Unlock()
		return false
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

// Stats reads the counters.
func (h *Hub) Stats() Stats {
	h.mu.Lock()
	streams, views := h.streams, len(h.views)
	h.mu.Unlock()
	return Stats{
		Ticks:         h.ticks.Load(),
		Renders:       h.renders.Load(),
		FramesSent:    h.sent.Load(),
		FramesDropped: h.dropped.Load(),
		Overruns:      h.overruns.Load(),
		Streams:       streams,
		Views:         views,
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
	} else if v.presence || v.frame.Load() == nil {
		marked = h.mark(v)
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
