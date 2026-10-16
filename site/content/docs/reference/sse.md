---
title: sse
headline: lith/sse: Datastar events, compressed per stream
summary: Writes Datastar server-sent events through a compressor of its own per stream.
description: Reference for lith/sse: Datastar's server-sent events, formatted once and compressed per stream with zstd, brotli or gzip within a memory budget.
order: 6
---

Import path `github.com/zweiundeins/lith/sse`. Generated from the doc comments in [sse/](https://github.com/zweiundeins/lith/tree/main/sse).

Events are formatted once (PatchElements, PatchSignals, Comment) and the same bytes can go to any number of streams. Each stream compresses them with a window that spans the whole stream, so a frame that repeats much of an earlier one costs little on the wire. The encoders are tuned for many long-lived streams: zstd at its fastest level (Options.ZstdLevel) with a 512 KiB window (about 1.7 MiB of heap per stream, 1 MiB with Options.ZstdLowMemory) when the client accepts it, else brotli at quality 4 with a 256 KiB window (about 1.6 MiB), else gzip.

A Budget shares a memory limit among many streams, their encoders and the frames kept for them: each stream gets a window that holds its frames, zstd streams move to smaller windows as more streams open, and brotli streams take quality 3, then a window below their frames, as the limit fills. Stream.Hibernate lets an idle zstd stream give its encoder back until its next write, and Stream.Upgrade tells when a brotli stream would do better on a new connection.

Every write has its own deadline, so run the server without a WriteTimeout and let a stream that cannot take a write in time fail alone. Pages open their streams with Datastar's retry: 'error' from an element with an id: a server that ends its streams to restart sends Reconnect last (live does, with Spec.Reconnect), and 'error' carries the page's request through the restart. The default retry reopens neither.

## Constants

```go
const (
	Zstd   = "zstd"
	Brotli = "br"
	Gzip   = "gzip"
)
```

The content codings a stream can use, in the server's order of preference.

```go
const MaxSignals = 1 << 20
```

MaxSignals is the most bytes of signals ReadSignals takes from a request's body.

## Variables

```go
var ErrBodyConsumed = errors.New("sse: the request's body was read before ReadSignals: read the signals before Open")
```

ErrBodyConsumed is returned by ReadSignals when the request's body was read before it, as net/http does when Open sends the headers.

```go
var ErrClosed = errors.New("sse: stream closed")
```

ErrClosed is returned by writes to a closed stream.

```go
var ErrNoRoom = errors.New("sse: no room in the streams' budget")
```

ErrNoRoom is returned by Open when the budget has no room for the stream and BudgetOptions.Refuse is set. Open has written nothing: the handler answers 503, and a page that opened the stream with retry 'error' tries again (live's Serve answers it).

## Functions

### func Comment {#Comment}

```go
func Comment(text string) []byte
```

Comment formats an SSE comment, which the page ignores (a heartbeat).

### func Negotiate {#Negotiate}

```go
func Negotiate(acceptEncoding string) string
```

Negotiate picks the coding for an Accept-Encoding header: the acceptable one with the highest q-value, zstd before brotli before gzip when they tie, or "" for none.

### func PatchElements {#PatchElements}

```go
func PatchElements(html []byte, p ...Patch) []byte
```

PatchElements formats a datastar-patch-elements event. It takes at most one Patch; without one, the elements are morphed in by their ids.

### func PatchSignals {#PatchSignals}

```go
func PatchSignals(json []byte, onlyIfMissing bool) []byte
```

PatchSignals formats a datastar-patch-signals event with a JSON merge patch. With onlyIfMissing, the page keeps signals it already has.

### func ReadSignals {#ReadSignals}

```go
func ReadSignals(w http.ResponseWriter, r *http.Request, v any) error
```

ReadSignals decodes the Datastar signals of r into v. Datastar sends them as the datastar query parameter for GET and DELETE (and ReadSignals reads HEAD like GET), and as a JSON body for every other method, @query's QUERY included. A missing parameter or an empty body is no signals: v stays as it is and the error is nil. A body over MaxSignals bytes fails with an \*http.MaxBytesError, bad JSON with the decoder's error.

ReadSignals reads the body to its end: over HTTP/1.1, net/http notices that a client has left only once the body has been read, or discarded when the response's headers go out. So call it before Open, and before a handler waits on its context with nothing written.

Unlike datastar-go's ReadSignals(r, v), it takes w for http.MaxBytesReader, caps the body and accepts an empty one.

### func ReadSignalsLimit {#ReadSignalsLimit}

```go
func ReadSignalsLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) error
```

ReadSignalsLimit is ReadSignals with a limit of its own on the body.

### func Reconnect {#Reconnect}

```go
func Reconnect(id string) []byte
```

Reconnect is the last event of a stream that ends because its server restarts. It appends to the body an element that replaces the element with the given id by a copy of it, so that the copy's data-init, the @get or @post that opened the stream, runs again with its own options. That request must use retry 'error': Datastar 1.0.4 retries it while the restart answers 502 or 503, where the default 'auto' gives up. A stream that ends cleanly without Reconnect stays closed.

## Types

### type Budget {#Budget}

```go
type Budget struct {
	// contains filtered or unexported fields
}
```

Budget shares a memory limit among the streams that open with it through Options.Budget, so that a server holds what its memory allows, however many tabs are open.

A stream books what it holds from Open to Close: its encoder's tier, and 32 KiB for the rest of it. A live hub books the frames it keeps, once per view however many streams watch it (Hold). A stream's tier is picked at its first write, from the largest write it has made: a tier's window must hold that frame with a sixteenth to spare, or a frame cannot refer to the one before, and its bytes grow tens of times over. With BudgetOptions.Refuse, Open turns away a stream that has no room for a window that holds its frame (Options.Frame), and every stream it lets in compresses with such a window until it closes, unless its frames outgrow it.

A brotli stream takes the smallest window that holds its frame, since a larger one sends no fewer bytes: at its quality (4) while the budget stays at most half full, else at quality 3, and only when that does not fit what is left, quality 3 at 2^16, which compresses each frame alone. It keeps its tier until it closes. Open reserves quality 3 at 2^16 for it, and a stream that cannot have even that goes out uncompressed. With Refuse, Open reserves quality 3 at the window that holds its frame, or turns it away, and the stream never takes a window below that. A stream that holds less than its frame needs may be asked to open again once room frees up (Stream.Upgrade).

A zstd stream takes the largest tier within its fair share (what the limit leaves after frames and streams, divided by the streams that compress or would) and what is left, down to the smallest window that holds its frame, since zstd's bytes keep falling as its window grows. A stream whose fair share is below that gets it while what is left holds it, then the smallest window, 2^15, and else none: it sends raw blocks until room frees up. When a stream finds no room for a window that holds its frame, every zstd stream above the fair share of that moment moves down at its next write, but not below its frame's window: it ends its zstd frame and begins the next with the smaller tier, which costs one frame without history. A zstd stream whose window is below its frame moves up the same way once room frees up. Close gives the reservation back, and so does a zstd stream that hibernates (Stream.Hibernate). With Refuse, a zstd stream keeps the smallest tier whose window holds its frame booked from Open to Close, while it has no encoder too, and takes that tier when nothing larger fits: it moves no lower and never sends raw blocks. When the budget turns a stream away, the zstd streams within their fair share move down too, to the window that holds their frame, until what is left would hold the stream turned away.

The tiers' sizes are the most a stream held at each, after sessions of frames of 50 to 230 KB (lith's bench/budget, framesize.csv), rounded up to 16 KiB: zstd at its fastest level, brotli at quality 3 and 4. Other levels and qualities hold more.

#### func NewBudget {#NewBudget}

```go
func NewBudget(o BudgetOptions) *Budget
```

NewBudget returns a budget for streams to share through Options.Budget.

#### func (*Budget) Hold {#Budget.Hold}

```go
func (b *Budget) Hold(n int64)
```

Hold books n more bytes that the streams hold besides their encoders, or gives back -n: a live hub books the newest frame of each view that two or more streams watch, its length and 8 KiB, once for all of them. What Hold books counts against the limit and leaves the encoders less room, but is never refused: once it takes the whole limit, streams go out uncompressed, or with Refuse new ones are turned away while those open keep the windows that hold their frames.

#### func (*Budget) SetLimit {#Budget.SetLimit}

```go
func (b *Budget) SetLimit(n int64)
```

SetLimit changes the budget's limit in bytes (0 for none), as when the process nears its memory limit. The zstd streams above their fair share under the new limit move down at their next write, or, when it leaves no room for them, send their frames uncompressed until it does (with Refuse, they move no lower than the window that holds their frame, and the budget turns new streams away until it has room). Brotli and gzip streams keep their tiers until they close.

#### func (*Budget) Stats {#Budget.Stats}

```go
func (b *Budget) Stats() BudgetStats
```

Stats reads the budget.

### type BudgetOptions {#BudgetOptions}

```go
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
	// Refuse turns away a stream that the budget has no room for: Open
	// returns ErrNoRoom. A stream whose client accepts a coding needs room
	// for 32 KiB and the smallest tier of that coding whose window holds
	// its frame (Options.Frame, which live's Serve sets from the stream's
	// first frames; unset, the smallest tier the stream may take); one
	// that compresses nothing (its client accepts no coding, or
	// Options.NoCompression) for its 32 KiB. That tier is the least the
	// stream then holds: a zstd stream keeps it booked while it has no
	// encoder and moves no lower, a brotli stream takes no window below
	// its frame. A stream whose frames outgrow that window later keeps its
	// tier and sends more bytes a frame until it gets a larger one (zstd
	// once room frees up, brotli on a new connection, Stream.Upgrade); it
	// is never cut off. Without Refuse, a stream without room for a tier
	// goes out uncompressed, which suits small, rare frames, and one that
	// compresses nothing is never turned away. Large and frequent frames
	// are better refused, for pages that open their stream with retry
	// 'error' and so come back: the budget then caps the streams by what
	// they hold, and live's MaxStreams, set high, is only a backstop.
	Refuse bool
}
```

BudgetOptions configure a Budget. The zero value takes half the process's memory limit and gives each stream the window its frames need, up to 2^19.

### type BudgetStats {#BudgetStats}

```go
type BudgetStats struct {
	Limit int64 // bytes, 0 for none
	// Reserved is what the open streams hold: their tiers, 32 KiB each
	// besides, and Frames.
	Reserved int64
	Frames   int64 // booked through Hold: the frames a live hub keeps
	// Streams counts the open streams by what they hold: a tier such as
	// "zstd 2^19", "zstd 2^17 low memory" or "br q4 2^18"; "zstd asleep"
	// for zstd streams without an encoder (before their first write,
	// hibernating, or, without Refuse, waiting for room); "identity" for
	// streams that go out uncompressed.
	Streams  map[string]int
	Refused  int64 // streams turned away since NewBudget
	Reopened int64 // streams asked to open again for a better tier (Stream.Upgrade)
}
```

BudgetStats describe a budget at one moment.

### type Mode {#Mode}

```go
type Mode string
```

Mode is how patched elements are applied to the page.

```go
const (
	Outer   Mode = "outer" // morph the element with the same id (the default)
	Inner   Mode = "inner" // morph the selected element's children
	Replace Mode = "replace"
	Prepend Mode = "prepend"
	Append  Mode = "append"
	Before  Mode = "before"
	After   Mode = "after"
	Remove  Mode = "remove"
)
```

The patch modes of Datastar v1.

### type Namespace {#Namespace}

```go
type Namespace string
```

Namespace is the namespace of patched elements that are not HTML.

```go
const (
	SVG    Namespace = "svg"
	MathML Namespace = "mathml"
)
```

The namespaces Datastar v1 knows besides HTML.

### type Options {#Options}

```go
type Options struct {
	// WriteTimeout bounds each write to the client (default 10s). A
	// client that does not take a write in time loses its stream. The end
	// frame of a stream the hub stops waits 250 ms at most.
	WriteTimeout time.Duration
	// Compress, if set, is a semaphore shared by streams: its capacity
	// bounds how many compress at once. A slot is held only while
	// compressing, never during the network write.
	Compress chan struct{}
	// ZstdWindow is the base 2 logarithm of zstd's window (default 19).
	// With a Budget that has a limit, it is the largest window the budget
	// gives, and unset, the budget's MaxWindow.
	ZstdWindow int
	// ZstdLevel is zstd's level (default zstd.SpeedFastest). A higher one
	// sends fewer bytes per frame for more CPU and heap per stream:
	// bench/RESULTS.md has the trade-off measured on a shared board.
	ZstdLevel zstd.EncoderLevel
	// ZstdLowMemory keeps zstd's history at its window: about 1 MiB per
	// stream instead of 1.65 MiB, for about 40% more CPU per frame
	// (TestHeapPerStream), since the encoder then shifts its window after
	// every 128 KiB of input.
	ZstdLowMemory bool
	// BrotliQuality is brotli's quality, 1 to 11 (default 4).
	BrotliQuality int
	// BrotliWindow is the base 2 logarithm of brotli's window (default 18).
	// With a Budget that has a limit, it is the largest window the budget
	// gives, and unset, the budget's MaxWindow.
	BrotliWindow int
	// NoCompression sends the stream as it is, whatever the client accepts:
	// for small, frequent events, where an encoder's heap (0.4 MiB and up
	// per stream) costs more than it saves.
	NoCompression bool
	// Budget, if set, is shared by streams: it picks each stream's window
	// within a memory limit, from the size of its writes. ZstdLowMemory
	// keeps a stream to low-memory windows, and BrotliQuality is the
	// quality brotli streams take while the budget has room (else 3).
	Budget *Budget
	// Frame, if set, is the length in bytes of the stream's frames as far
	// as its handler knows them before Open: live's Serve sets it to the
	// length of the stream's first frames. A Budget with
	// BudgetOptions.Refuse lets the stream in only when it has room for a
	// window that holds such a frame, and picks the stream's tiers for
	// frames at least that long. Without Refuse it changes nothing.
	Frame int
}
```

Options tune a stream. The zero value is ready to use.

### type Patch {#Patch}

```go
type Patch struct {
	Selector               string
	Mode                   Mode
	Namespace              Namespace
	ViewTransition         bool
	ViewTransitionSelector string // used only with ViewTransition
}
```

Patch says where and how patched elements go. The zero value morphs each element into the page by its id. Values must be single lines.

ViewTransition morphs inside a view transition, the whole document's unless ViewTransitionSelector names an element to scope it to. Scope it: while a transition of the document runs, Chromium gives every press on the page to \<html>, and buttons and links miss their clicks. A browser without scoped transitions morphs at once.

### type Stream {#Stream}

```go
type Stream struct {
	// contains filtered or unexported fields
}
```

Stream is one open event stream. Its methods are not safe for concurrent use, except that the stream may end (its request context ends) at any time: a write in progress then fails at once.

#### func Open {#Open}

```go
func Open(w http.ResponseWriter, r *http.Request, o Options) (*Stream, error)
```

Open starts an event stream on w: it picks the encoding from the request's Accept-Encoding, sends the headers and flushes them. The stream ends when the request's context ends. The handler must Close the stream before it returns.

#### func (*Stream) Budget {#Stream.Budget}

```go
func (s *Stream) Budget() *Budget
```

Budget is the budget the stream shares with other streams (Options.Budget), nil for none.

#### func (*Stream) Close {#Stream.Close}

```go
func (s *Stream) Close() error
```

Close ends the compressed stream cleanly when it is still healthy and releases the encoder. It does not end the HTTP response: the handler does, by returning.

#### func (*Stream) CloseWith {#Stream.CloseWith}

```go
func (s *Stream) CloseWith(events ...[]byte) error
```

CloseWith is Close that sends events in the stream's last frame. It sends them also when the stream shuts down (the hub stops), as live does with Reconnect.

#### func (*Stream) Encoding {#Stream.Encoding}

```go
func (s *Stream) Encoding() string
```

Encoding is the stream's content coding: Zstd, Brotli, Gzip or "".

#### func (*Stream) Hibernate {#Stream.Hibernate}

```go
func (s *Stream) Hibernate() error
```

Hibernate ends the stream's zstd frame and gives its encoder to a pool shared by streams, and its tier back to its budget, so that an idle stream holds neither. The next write takes an encoder from the pool and starts a new frame, without the history of the frames before; with a budget, of the tier the budget then allows, or in raw blocks while it allows none. A budget with BudgetOptions.Refuse keeps the least tier that holds the stream's frame booked for it, which its next write takes when no larger one fits. A Content-Encoding: zstd body may hold any number of frames one after another, and browsers decode them as one stream. Hibernate does nothing to a stream of another coding: a brotli or gzip body is a single stream, which cannot start over without its history.

#### func (*Stream) Sent {#Stream.Sent}

```go
func (s *Stream) Sent() (events, wire int64)
```

Sent reports the bytes of events written to the stream and the bytes that went out for them, compressed.

#### func (*Stream) Tier {#Stream.Tier}

```go
func (s *Stream) Tier() (name string, heap int64)
```

Tier names what the stream's encoder holds now, as BudgetStats.Streams counts it: "zstd 2^19", "zstd 2^18 low memory", "br q4 2^18" or "gzip"; "zstd asleep" while a zstd stream has no encoder (before its first write, while it hibernates, or while its budget has no room for it); "identity" when it goes out uncompressed. heap is what the stream's budget reserves for the encoder: 0 without a budget, and for a stream asleep or uncompressed, but for an asleep one, with BudgetOptions.Refuse, the heap of the least tier that holds its frame. A brotli stream with a budget holds quality 3 at 2^16 until its first write picks its tier, with Refuse quality 3 at the window that holds its frame.

#### func (*Stream) Upgrade {#Stream.Upgrade}

```go
func (s *Stream) Upgrade() <-chan struct{}
```

Upgrade returns a channel that the stream's budget closes when a stream opened now would get a tier whose window holds this stream's frames, and this one's does not: a brotli stream whose window is below its frames, or one that went out uncompressed for want of room. A brotli stream keeps its tier while it is open, so it gets the better one only by opening again. The budget closes the channel of at most one stream a second. A handler that can have the page open its stream again ends the stream then, with Reconnect: live's does for a stream with Spec.Reconnect, but not twice within a minute for the same Spec.ID. Without a budget, and for streams that move while open (zstd) or never would (gzip), the channel stays open.

#### func (*Stream) Write {#Stream.Write}

```go
func (s *Stream) Write(events ...[]byte) error
```

Write sends events: it compresses them together, flushes the encoder and writes the result to the client within the write timeout. A write the client does not take in time fails with an error that wraps os.ErrDeadlineExceeded, and one cut short by the end of the request's context with the context's cause. After a failed write the stream is broken, and every later write returns the same error.
