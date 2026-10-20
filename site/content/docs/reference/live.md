---
title: live
headline: lith/live: views rendered once a tick, streamed
summary: Renders views from the database and streams them to pages: view = f(state).
description: Reference for lith/live: views keyed by topics, rendered once per tick in a read transaction, and streamed to every page that watches them.
order: 5
---

Import path `github.com/zweiundeins/lith/live`. Generated from the doc comments in [live/](https://github.com/zweiundeins/lith/tree/main/live).

A View is a part of a page. Views with the same key are one view, which renders once for every stream that watches it. A commit marks views dirty through their topics (Changed, wired to the writer's OnCommit). The hub renders dirty views in ticks: its loop sleeps while nothing is dirty, starts a tick at once after a quiet spell, and runs at most one tick per Options.Tick. In a tick the dirty views go onto one queue that the render workers drain. Each worker holds a connection of the read pool and renders all its views of the tick in one read transaction. When every worker is done, the new frames go out.

A view renders in one read transaction, so it agrees with itself. Views rendered by different workers can see different commits when one lands during the tick; that commit marks its views dirty, and the next tick brings them together. What must always agree belongs in one view.

A view's topics are its Topics and those its last Render declared with Depends, so a view whose reads follow a row, such as a tab's route, follows them without a new key. Topics form a tree by their segments: a commit to src:A:7 marks the views of src:A and src as well, and one to src:A:\* those of everything below src:A. A commit after a Render's snapshot, to a topic that Render declares for the first time, marks the view dirty once the hub takes the topic on.

The hub numbers the commits Changed reports, 1, 2, 3 and on (Seq), and keeps the number of the last commit that touched each topic (Version): a cache of something a Render or a GET computes, such as a count, stays good while the version of its topics is not above the number it was computed at (Seq(ctx) in a Render).

A stream keeps only the newest frame of each view it watches, so a slow client skips frames and holds up no one else. Views that change in the same tick reach a stream together, in the order it watches them. A stream to a client on a slow link (Spec.Slow) takes a view's frames about four times a second.

A dirty view does not render while every stream that watches it still has a frame of it waiting: a stream that has not sent the last one has no use for the next. The hub defers the view instead (Stats.RendersDeferred), and the first stream that takes its waiting frame marks the view dirty again, so the next tick renders the state as it is then. A view stays deferred, and costs no render, while its streams' writes block, and a view that only slow streams watch renders about four times a second. A view that one stream keeps up with renders at every change, for all of its streams.

The hub keeps a view's newest frame while two or more streams watch it, so that a stream that joins gets it at once. Of a view that one stream watches, such as a tab's own, it keeps only the frame's length and hash once the stream has the frame: a render that repeats it goes to no stream, and a stream that joins gets a frame rendered in the next tick. A frame that is not \[]byte has no hash, and the hub keeps it.

## Constants

```go
const AckKey = "live.ack"
```

AckKey is the key of the Frame that carries a stream's acknowledgement event (Spec.Ack) to WriteTick or Write.

```go
const AckTimeout = 10 * time.Second
```

AckTimeout is how long a stream with Spec.Window waits for an acknowledgement before it sends its next write anyway.

```go
const DefaultRelease = 5 * time.Second
```

DefaultRelease is Spec.Release when it is 0: a paced page whose acknowledgements all come in time for 5 s is released.

```go
const DefaultReleaseSlack = 100 * time.Millisecond
```

DefaultReleaseSlack is Spec.ReleaseSlack when it is 0: a page that applies a frame within 100 ms of its round trip keeps up with ten frames a second.

```go
const NewestFrame = "newest-frame.js"
```

NewestFrame names newest-frame.js in Scripts, a Datastar plugin that does in the page what the hub does for a stream: the newest frame of a view wins. lith vendors its build from github.com/mbolli/datastar-attribute-newest-frame, which has its source, tests and documentation; VERSION next to it names the commit. A page loads it as a module after Datastar, through an import map that maps 'datastar' to Datastar's URL, and turns it on with data-newest-frame on its \<body>.

It holds the patch events of the stream that the element #stream opened (data-newest-frame:feed\_\_reopen names another id), and passes them on to Datastar once each chunk of the stream is parsed, or at the next animation frame when it passed some on less than a frame before, and no sooner after a batch than half the time that batch took to apply. Of the element patches in such a batch that replace the same elements wholly, only the newest is applied: mode outer or replace of elements that all have ids, or mode inner with an id selector, without a script or a view transition. Signal patches and every other patch keep their order and are never dropped. A page that keeps up applies every frame.

A page whose main thread is too slow for its stream falls behind, and the hub cannot tell: the browser and the kernel hold minutes of frames, and the server's writes never wait. Chromium then hands the page 2 MiB of the stream at each read, and Datastar applies all of it in one task without a paint between. Firefox holds the frames in its parent process and hands the page one at a time while its main thread is free, which the pause after a long batch leaves time for. With the plugin the page applies each view's newest frame instead and stays within about a second. When more than half of the frames of the last 3 s were dropped, at least 10, it dispatches newest-frame-behind on the stream's element, again every 3 s while the page stays behind. A listener on \<body>, never on the stream's element, whose @post would abort the stream, can post it to a route that calls Hub.Behind, which counts the pages that fall behind.

With data-newest-frame\_\_reopen it also sets $\_slow and opens the stream again from a copy of its element. The page's stream sends Save-Data: on while $\_slow is true, so the handler's edge.SlowLink gives it a slow stream (Spec.Slow) at the same URL, and Datastar aborts the old request. It asks once; a page that measured a slow link as it started never asks. It is not the default: the slow stream's four frames a second are fixed, fewer than a page only somewhat slow applies without it, and the page never switches back.

## Variables

```go
var (
	// ErrFull is returned by Serve when MaxStreams streams are open.
	ErrFull = errors.New("live: too many streams")
	// ErrClientFull is returned by Serve when the stream's Spec.Client
	// holds Options.MaxStreamsPerClient streams already.
	ErrClientFull = errors.New("live: too many streams from this client")
	// ErrStopped is returned by Serve once the hub has stopped.
	ErrStopped = errors.New("live: hub stopped")
)
```

```go
var Scripts embed.FS
```

Scripts holds the scripts lith gives the page: newest-frame.js (NewestFrame). Serve them with the app's other static files, by a name edge.Static makes from their content and as immutable:

```go
js, err := fs.ReadFile(live.Scripts, live.NewestFrame)
if err != nil {
	return err
}
static.Add("lith/"+live.NewestFrame, js)
url := static.URL("lith/" + live.NewestFrame) // /static/lith/newest-frame.3f2a9c1b2d4e.js
```

## Functions

### func Below {#Below}

```go
func Below(t string) string
```

Below is the topic a commit touches to say that everything at or below t changed: t with :\*, for Changed and writer.Tx.Touch.

### func Depends {#Depends}

```go
func Depends(ctx context.Context, topics ...string)
```

Depends declares, in a Render, that the view read what the topics name, as Topics do: a commit that touches one of them marks the view dirty. The topics the Render declares replace those the Render before it declared, once it returns, so a view whose reads depend on a row, such as a tab's route, declares what it read for the route it rendered, and a commit to what an earlier route read no longer renders it. A Render that fails keeps the topics of the one before. A commit that lands after the Render's snapshot and touches a topic the view takes on marks it dirty as well: the hub compares the topic's Version with Seq(ctx). A topic that View.Topics would refuse panics, which fails the Render. Outside a Render, Depends does nothing.

### func FrameBuffer {#FrameBuffer}

```go
func FrameBuffer(ctx context.Context) []byte
```

FrameBuffer is, in a Render, an empty buffer for the view's frame, with room for its last \[]byte frame and an eighth more: a Render that formats its frame into it (sse.AppendPatchElements, sse.NewElements) allocates the frame once while it keeps about its size. It is nil before the view's first \[]byte frame, and outside a Render.

### func Seq {#Seq}

```go
func Seq(ctx context.Context) uint64
```

Seq is, in a Render, the number of the last commit its snapshot is sure to hold (Hub.Seq): the hub reads it before the read transaction begins, so a later commit may be in the snapshot as well. A value the Render computes from its snapshot is good as of that number: keep it while Hub.Version of the topics it reads is not above it.

```go
if c, ok := counts.Get(space); ok && hub.Version("space:"+space) <= c.seq {
	return c.n
}
n := countItems(ctx, q, space)
counts.Add(space, count{n: n, seq: live.Seq(ctx)})
```

Outside a Render it is 0, which keeps a value good only while none of its topics has changed since the hub started. A GET reads Hub.Seq before its read transaction begins instead.

### func Topic {#Topic}

```go
func Topic(parts ...string) string
```

Topic joins parts into a topic, one segment each: Topic("src", space, uid). It escapes ':', '\*' and '%' in a part as %3A, %2A and %25, so an id from outside the app, such as urn:uuid:..., stays one segment and never reads as a level or as everything below. A part without them comes out as it is, so Topic("board", "42") is board:42. An empty part becomes %, which no other part escapes to.

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

#### func (*Hub) Ack {#Hub.Ack}

```go
func (h *Hub) Ack(id, token string, n int64) bool
```

Ack acknowledges the writes of the stream with the ID and token up to the nth (Spec.Window), and frees the window for as many. It reports whether such a stream is open. An acknowledgement of an older write, or of one not written yet, changes nothing.

With Spec.Release, it also notes whether a frame was waiting for the acknowledgement, and ends the pacing once none has for long enough.

An app's route that calls Ack must check that the stream belongs to the request's session and limit how often a client may call it.

#### func (*Hub) Behind {#Hub.Behind}

```go
func (h *Hub) Behind(id string) bool
```

Behind counts a report that the page of the open stream with the ID fell behind it, as newest-frame.js tells with newest-frame-behind, in Stats.Behind, and the stream's first report in Stats.BehindStreams. It counts one report per stream every 2 s at most, so a page that sends more cannot inflate the counts, and none for an ID with no open stream. It reports whether it counted.

#### func (*Hub) Budget {#Hub.Budget}

```go
func (h *Hub) Budget() *sse.Budget
```

Budget is the budget the hub's streams share: Options.SSE.Budget, or the one New made. Its Stats tell what the streams hold, the frames the hub keeps among it, and SetLimit moves its limit while the hub runs.

#### func (*Hub) Changed {#Hub.Changed}

```go
func (h *Hub) Changed(topics []string)
```

Changed marks dirty every view with one of the topics, or with a topic above one: a commit to src:A:7 touches src:A and src as well. A topic that ends in :\*, such as src:A:\*, touches everything at or below src:A, and src above it. A topic of \* alone touches everything, as MarkAll. A topic or one above it that the topics name twice counts once. Changed does not check the topics, as Watch does: it runs for every commit.

It is meant for the writer's OnCommit and never waits for a render. Each call is a commit: it takes the next number (Seq), which becomes the version of the topics it touches. Call it after the change is in the database, as OnCommit is. An app that commits through its own store calls it after each commit is visible to readers, never inside the transaction: a render that begins after Changed counts the commit as part of its snapshot.

#### func (*Hub) Count {#Hub.Count}

```go
func (h *Hub) Count(key string) int
```

Count is the number of streams watching the view.

#### func (*Hub) Done {#Hub.Done}

```go
func (h *Hub) Done() <-chan struct{}
```

Done is closed once the hub stops: when Run's ctx ends, or Run returns. Work that only serves the hub's views ends with it, such as a clock that changes a view while someone watches it (Count).

#### func (*Hub) HasStream {#Hub.HasStream}

```go
func (h *Hub) HasStream(id string) bool
```

HasStream reports whether a stream with the ID is open. An action that stores state for a tab can refuse a tab whose stream is not open, so that a client cannot make rows for tab ids it made up.

#### func (*Hub) MarkAll {#Hub.MarkAll}

```go
func (h *Hub) MarkAll()
```

MarkAll marks every view dirty, as after a change nothing tracks (a migration, new templates). It counts as a commit that touched every topic: Version then reports it for each. It is Changed with \*.

#### func (*Hub) Pace {#Hub.Pace}

```go
func (h *Hub) Pace(id string, window int) bool
```

Pace sets the window of the open stream with the ID (Spec.Window): a positive window paces it by the page's acknowledgements from its next write on, and 0 sends every frame again. The stream needs Spec.Ack. It reports whether such a stream is open. A stream paced already keeps its window and its count towards Spec.Release.

An app's route that calls Pace must check that the stream belongs to the request's session and limit how often a client may call it.

#### func (*Hub) RoundTrip {#Hub.RoundTrip}

```go
func (h *Hub) RoundTrip(id string, rtt time.Duration)
```

RoundTrip notes a round trip the page of the open stream with the ID measured, such as from the Resource Timing of its last acknowledgement. The stream keeps the shortest (Spec.ReleaseSlack).

#### func (*Hub) Run {#Hub.Run}

```go
func (h *Hub) Run(ctx context.Context) error
```

Run renders views until ctx ends. Then it ends every stream: a write under way fails at once, and any other stream ends its response cleanly, compressed stream and all, within its write timeout. Run returns nil when its workers are done; shutdown never waits for a stream. It returns an error only when it cannot start.

#### func (*Hub) Seq {#Hub.Seq}

```go
func (h *Hub) Seq() uint64
```

Seq is the number of the last commit: Changed and MarkAll number them from 1, and 0 is the hub's start. The numbers start again with every hub, so keep nothing keyed by them beyond the process.

A cache that a GET fills reads Seq before its read transaction begins: every commit up to that number is in the snapshot, and a later one may be too. The value it computes is good as of that number, and stays good while Version of its topics is not above it. Read after the snapshot, the number could claim a commit the snapshot does not hold. A Render takes the number from Seq(ctx).

#### func (*Hub) Serve {#Hub.Serve}

```go
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, spec Spec) error
```

Serve streams the views of spec to the client until it leaves or the hub stops, and then returns nil. It sends each view's newest frame when the hub keeps it (View.Render), else a frame rendered in the next tick, then every new frame. It opens the stream once those first frames are there, or the tick that renders them has ended, and passes their length to the streams' budget (sse.Options.Frame), so the response's headers go out with the first frames: a budget with sse.BudgetOptions.Refuse lets the stream in only with room for a window that holds them. When the hub is full or stopped, or the streams' budget refuses the stream, it answers 503 and returns ErrFull, ErrStopped or sse.ErrNoRoom; a failed write ends the stream with its error.

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

#### func (*Hub) Version {#Hub.Version}

```go
func (h *Hub) Version(topics ...string) uint64
```

Version is the number of the last commit that touched one of the topics (Seq), 0 when none did since the hub started. A commit touches a topic as Changed says: Version of src:A moves with every commit to src:A:7, and Version of src:A:7 with one to src:A:\*. A topic with :\* reads as the topic without it, and \* as Seq. A topic the hub no longer keeps (Options.Versions, Options.PrefixVersions) reads as changed at the commit that pushed it, or a topic above it, out of the hub, or at the last MarkAll; so do no topics.

#### func (*Hub) Watch {#Hub.Watch}

```go
func (h *Hub) Watch(streamID string, v View) bool
```

Watch makes the open stream with this ID watch v as well: it gets v's newest frame at once when the hub keeps it (View.Render), else a frame rendered in the next tick. It reports whether such a stream is open. If none is, as when a page's command arrives before its stream, a stream that opens with the ID within Options.PendingWatch watches v from the start, after the views of its Spec.

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
	// Serve answers 503 to more. With a budget that refuses streams
	// (sse.BudgetOptions.Refuse), memory caps them, and MaxStreams is a
	// backstop.
	MaxStreams int
	// MaxStreamsPerClient is how many streams one client, by Spec.Client,
	// may hold open at once (default 16; negative: no cap). Serve answers
	// 429 to more. Without it one client can take every place under
	// MaxStreams with idle streams that cost it nothing, and every other
	// page gets 503; and since the streams' budget shares its memory per
	// stream, the client that came first keeps the compressed tiers while
	// later ones go out uncompressed. A browser holds one stream per tab:
	// 16 leaves room for a person with many tabs, or a few people behind
	// one address, while it takes MaxStreams/16 addresses to fill the hub.
	// An app whose users share one address (an office, a VPN) keys a
	// signed-in user by its account instead, or raises the cap.
	MaxStreamsPerClient int
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
	// when not even the smallest tier fits. A process without a memory
	// limit gives the streams a quarter of the machine's memory, and New
	// logs a warning; New logs the budget's limit in any case. The budget
	// shares its room per stream: one client that opens many streams
	// first would keep the compressed tiers, which MaxStreamsPerClient
	// prevents. Pass a Budget of your own for another limit, or with Refuse,
	// which Serve answers with 503 when the budget has no room for a
	// stream: for a window that holds its first frames, or for the 32 KiB
	// of one that compresses nothing; or one with Limit -1 to turn it off.
	SSE sse.Options
}
```

Options configure a Hub. The zero value of every field but Read selects its default.

### type Spec {#Spec}

```go
type Spec struct {
	// Client is the key that Options.MaxStreamsPerClient counts the
	// stream under: edge.ClientKey(r) for anonymous visitors, or a
	// signed-in user's account where users share an address. A session id
	// is no such key: a client mints a new one with every request that
	// sends no cookie. Empty: the stream counts under no client.
	Client string
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
	// process. So it does when the streams' budget has room for a better
	// tier than the stream holds, which a brotli stream takes only on a new
	// connection (sse.Stream.Upgrade): once a minute at most for the same
	// ID, and for a stream without an ID only after a minute open. Empty:
	// the stream just ends, as it does when a newer stream with its ID
	// replaces it.
	Reconnect string
	// Slow marks a stream to a client on a slow link (edge.SlowLink tells
	// one). It sends the newest frame of a view 250 ms or more after the
	// last, four a second. A frame is due at once when it is its view's
	// first, or its view sent nothing for 250 ms, and it takes the frames
	// that wait along: views that change together still arrive together,
	// and a view that changes now and then is not held up, for one frame
	// more of the others with each of its own. While a frame waits, a view
	// that no other stream keeps up with is not rendered
	// (Stats.RendersDeferred), so it renders about four times a second,
	// and its frame can be up to 250 ms old when it goes out; a view that
	// others keep up with renders for them, and the frames in between drop
	// (Stats.FramesDropped). Heartbeats go out as before.
	Slow bool
	// Window, Ack, Release and ReleaseSlack pace a stream by its page.
	// A stream with Ack alone is paced as lith's template paces it: it
	// starts unpaced, Hub.Pace paces it once its page reports that it is
	// behind, and the defaults of Release and ReleaseSlack end the pacing
	// once the page keeps up again.
	//
	// Window, if positive, paces the stream by the page: at most Window
	// writes of frames go out that the page has not acknowledged with
	// Hub.Ack. Each write of frames ends with the event Ack makes for it,
	// which has the page acknowledge the write once it has applied it.
	// While Window writes wait for theirs, the stream's frames wait too:
	// a view that no other stream keeps up with is not rendered
	// (Stats.RendersDeferred), and a view that others keep up with drops
	// the frames in between (Stats.FramesDropped). When an
	// acknowledgement frees the window, a waiting frame whose view was
	// deferred since is dropped, and the next tick renders the view as it
	// is then, so the page never gets a frame that waited for it. A
	// stream that hears nothing for AckTimeout sends its next write
	// anyway. It needs an ID and Ack. A stream with Ack and no Window
	// starts unpaced, and Hub.Pace can pace it later.
	Window int
	// Ack makes the event that ends a write of frames on a stream with
	// Window. The event has the page call Hub.Ack with the stream's ID,
	// token and n once it has applied the write: n counts the stream's
	// paced writes of frames from 1, and token tells this stream from an
	// older or newer one with the same ID. It goes out as the last Frame
	// of WriteTick's call, with the key AckKey, or the last event of the
	// write.
	Ack func(token string, n int64) []byte
	// Release ends the pacing of a stream whose page keeps up: once no
	// acknowledgement for Release came after the stream's next frame was
	// ready (that is, the page was never the reason a frame waited), the
	// stream stops asking for them and sends every frame again
	// (Stats.Releases). Each release of the same stream doubles the time
	// for the next, up to eight times Release, so that a page at the edge
	// of keeping up, paced again by Hub.Pace, does not switch back and
	// forth every Release. 0 takes DefaultRelease; a negative Release
	// keeps the stream paced until Hub.Pace sets its window to 0.
	Release time.Duration
	// ReleaseSlack changes what Release counts as late once the page has
	// reported its round trip (Hub.RoundTrip): an acknowledgement that
	// came more than ReleaseSlack after the round trip, from the write it
	// acknowledges, instead of one that came after the next frame was
	// ready. A page on a link whose round trip is longer than a view's
	// tick can then still be released once it applies its frames within
	// ReleaseSlack. 0 takes DefaultReleaseSlack; a negative slack keeps
	// the rule of the next frame even when the page reports its round
	// trip. Until the page reports one, the rule of the next frame holds.
	ReleaseSlack time.Duration
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
```

Stats count the hub's work since it started.

### type View {#View}

```go
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
```

View is a part of a page that the hub renders and streams.
