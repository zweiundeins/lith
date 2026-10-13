---
title: live
summary: Renders views from the database and streams them to pages: view = f(state).
order: 3
---

Import path `github.com/zweiundeins/lith/live`. Generated from the doc comments in [live/](https://github.com/zweiundeins/lith/tree/main/live).

A View is a part of a page. Views with the same key are one view, which renders once for every stream that watches it. A commit marks views dirty through their topics (Changed, wired to the writer's OnCommit). The hub renders dirty views in ticks: its loop sleeps while nothing is dirty, starts a tick at once after a quiet spell, and runs at most one tick per Options.Tick. In a tick the dirty views go onto one queue that the render workers drain. Each worker holds a connection of the read pool and renders all its views of the tick in one read transaction. When every worker is done, the new frames go out.

A view renders in one read transaction, so it agrees with itself. Views rendered by different workers can see different commits when one lands during the tick; that commit marks its views dirty, and the next tick brings them together. What must always agree belongs in one view.

A stream keeps only the newest frame of each view it watches, so a slow client skips frames and holds up no one else. Views that change in the same tick reach a stream together, in the order it watches them.

## Variables

```go
var (
	// ErrFull is returned by Serve when MaxStreams streams are open.
	ErrFull = errors.New("live: too many streams")
	// ErrStopped is returned by Serve once the hub has stopped.
	ErrStopped = errors.New("live: hub stopped")
)
```

## Types

### type Cache {#Cache}

```go
type Cache[K comparable] struct {
	// contains filtered or unexported fields
}
```

Cache keeps rendered fragments by a key that stands for what they show (content-addressed: a card's id, version and day), bounded in bytes. A fragment whose key comes again is copied instead of rendered.

It admits by frequency, after W-TinyLFU (Caffeine): a new fragment waits in a small window, and enters the main cache only if its key has been asked for more often than the fragment it would push out. Keys seen once (a component whose input always changes) never displace the ones that come back every tick, and fragments nobody asks for any more lose their place to ones that are. Safe for concurrent use.

#### func NewCache {#NewCache}

```go
func NewCache[K comparable](maxBytes int) *Cache[K]
```

NewCache returns a cache that holds at most maxBytes of fragments.

#### func (*Cache[K]) Get {#Cache.Get}

```go
func (c *Cache[K]) Get(key K, render func() []byte) []byte
```

Get returns the fragment for key, from the cache or else from render. Render runs without a lock held, and must return the same bytes for the same key; the caller must not change them.

#### func (*Cache[K]) Stats {#Cache.Stats}

```go
func (c *Cache[K]) Stats() CacheStats
```

Stats reads the counters.

### type CacheStats {#CacheStats}

```go
type CacheStats struct {
	Hits, Misses int64
	Rejected     int64 // fragments the admission turned away
	Entries      int
	Bytes        int
}
```

CacheStats count a cache's work.

### type Frame {#Frame}

```go
type Frame struct {
	Key   string
	Value any
}
```

Frame is a view's new frame, for Spec.WriteTick.

### type Hub {#Hub}

```go
type Hub struct {
	// contains filtered or unexported fields
}
```

Hub renders views and streams them. Create it with New and start it with Run.

#### func New {#New}

```go
func New(o Options) *Hub
```

New returns a hub; Run starts it.

#### func (*Hub) Budget {#Hub.Budget}

```go
func (h *Hub) Budget() *sse.Budget
```

Budget is the budget the hub's streams share: Options.SSE.Budget, or the one New made. Its Stats tell what the encoders hold, and SetLimit moves its limit while the hub runs.

#### func (*Hub) Changed {#Hub.Changed}

```go
func (h *Hub) Changed(topics []string)
```

Changed marks dirty every view with one of the topics. It is meant for the writer's OnCommit and never waits for a render.

#### func (*Hub) Count {#Hub.Count}

```go
func (h *Hub) Count(key string) int
```

Count is the number of streams watching the view.

#### func (*Hub) MarkAll {#Hub.MarkAll}

```go
func (h *Hub) MarkAll()
```

MarkAll marks every view dirty, as after a change nothing tracks (a migration, new templates).

#### func (*Hub) Run {#Hub.Run}

```go
func (h *Hub) Run(ctx context.Context) error
```

Run renders views until ctx ends. Then it ends every stream: a write under way fails at once, and any other stream ends its response cleanly, compressed stream and all, within its write timeout. Run returns nil when its workers are done; shutdown never waits for a stream. It returns an error only when it cannot start.

#### func (*Hub) Serve {#Hub.Serve}

```go
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, spec Spec) error
```

Serve streams the views of spec to the client until it leaves or the hub stops, and then returns nil. It sends each view's current frame (a view without one renders in the next tick), then every new frame. When the hub is full or stopped, or the streams' budget refuses the stream, it answers 503 and returns ErrFull, ErrStopped or sse.ErrNoRoom; a failed write ends the stream with its error.

#### func (*Hub) Stats {#Hub.Stats}

```go
func (h *Hub) Stats() Stats
```

Stats reads the counters.

#### func (*Hub) Unwatch {#Hub.Unwatch}

```go
func (h *Hub) Unwatch(streamID, key string) bool
```

Unwatch stops the stream with this ID from watching the view: after it returns, the stream sends no frame of the view, unless it is writing one already. It reports whether the stream watched the view, or the view waited for the stream to open.

#### func (*Hub) Watch {#Hub.Watch}

```go
func (h *Hub) Watch(streamID string, v View) bool
```

Watch makes the open stream with this ID watch v as well: it gets v's current frame at once, or its first one. It reports whether such a stream is open. If none is, as when a page's command arrives before its stream, a stream that opens with the ID within Options.PendingWatch watches v from the start, after the views of its Spec.

#### func (*Hub) Watchers {#Hub.Watchers}

```go
func (h *Hub) Watchers(key string) []string
```

Watchers are the IDs of the streams watching the view, each once, in the order the streams opened; streams without an ID are left out. A view with Presence that calls it in its Render lists who is there.

### type Options {#Options}

```go
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
```

Options configure a Hub. The zero value of every field but Read selects its default.

### type Spec {#Spec}

```go
type Spec struct {
	// ID names the stream for Watch and Unwatch, usually the tab id. A
	// stream with the ID of one still open replaces it if its request
	// came later, and else ends at once. Empty: none.
	ID string
	// Views are the views the stream watches from the start. Views that
	// change in the same tick are sent in this order.
	Views []View
	// Write sends a view's frame to the stream, and may tailor it (send
	// only what this stream has not seen). Nil: frames are []byte events,
	// written as they are. An error ends the stream.
	Write func(s *sse.Stream, key string, frame any) error
	// WriteTick, if set, takes Write's place: it gets the new frames of
	// the stream's views at once, so that one s.Write compresses and
	// flushes them together, where each call of Write is a write of its
	// own. Each call has the newest frame of every view that changed
	// since the last one, in the order the stream watches them: one
	// tick's, or several ticks' when the stream fell behind. frames is
	// valid during the call only. An error ends the stream.
	WriteTick func(s *sse.Stream, frames []Frame) error
	// Reconnect is the id of the page's element whose data-init opens this
	// stream with retry 'error'. When the hub stops (a deploy), the stream
	// ends with sse.Reconnect, so the page opens it again on the next
	// process. Empty: the stream just ends, as it does when a newer stream
	// with its ID replaces it.
	Reconnect string
}
```

Spec describes a stream.

### type Stats {#Stats}

```go
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
```

Stats count the hub's work since it started.

### type View {#View}

```go
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
```

View is a part of a page that the hub renders and streams.
