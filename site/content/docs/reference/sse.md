---
title: sse
headline: lith/sse: Datastar events, compressed per stream
summary: Writes Datastar server-sent events through a compressor of its own per stream.
description: Reference for lith/sse: Datastar's server-sent events, formatted once and compressed per stream with zstd, brotli or gzip within a memory budget.
order: 4
---

Import path `github.com/zweiundeins/lith/sse`. Generated from the doc comments in [sse/](https://github.com/zweiundeins/lith/tree/main/sse).

Events are formatted once (PatchElements, PatchSignals, Comment) and the same bytes can go to any number of streams. Each stream compresses them with a window that spans the whole stream, so a frame that repeats much of an earlier one costs little on the wire. The encoders are tuned for many long-lived streams: zstd at its fastest level (Options.ZstdLevel) with a 512 KiB window (about 1.7 MiB of heap per stream, 1 MiB with Options.ZstdLowMemory) when the client accepts it, else brotli at quality 4 with a 256 KiB window (about 2 MiB), else gzip.

A Budget shares a memory limit among the encoders of many streams: a stream gets the largest window its share of the limit allows, zstd streams move to smaller windows as more streams open, and brotli streams open with smaller windows and quality 3 as the limit fills. Stream.Hibernate lets an idle zstd stream give its encoder back until its next write.

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

Budget shares a memory limit among the encoders of the streams that open with it through Options.Budget, so that a server holds what its memory allows, however many tabs are open: few streams compress with large windows, many with small ones.

A stream reserves the heap of its encoder when it opens: the largest tier that fits both its fair share (the limit divided by the open streams that compress or would, itself included) and what is left of the limit. Tiers are zstd's and brotli's windows from MaxWindow down to MinWindow; zstd's largest keeps its full history, and brotli's go on below MinWindow at quality 3, down to 2^16. A stream whose fair share is below its smallest tier gets that tier while what is left holds it. Close gives the reservation back, and so does a zstd stream that hibernates (Stream.Hibernate): it reserves again on its next write, at the fair share of that moment.

Streams that open while few are open get large tiers, and would keep them as more arrive. So when a stream finds no room, every zstd stream above the fair share of that moment (counting a refused stream as open) moves down at its next write: it ends its zstd frame and begins the next with the smaller tier, which costs one frame without history. Brotli and gzip streams keep their tier until they close, since a brotli body is one stream with one window: brotli steps down as streams open instead, from its quality's windows to quality 3 at 2^16. Once more than 20 streams are open, a brotli stream takes at most its largest tier times twice the share of the limit that is left: the largest tier while half the limit is left, smaller ones as it fills, so that a tier taken early does not spend the room of the streams to come.

The tiers' sizes were measured with zstd at its fastest level and brotli at quality 3 and 4 (lith's bench/budget), after a session of 142 KB frames, and are rounded up. Other levels and qualities hold more.

#### func NewBudget {#NewBudget}

```go
func NewBudget(o BudgetOptions) *Budget
```

NewBudget returns a budget for streams to share through Options.Budget.

#### func (*Budget) SetLimit {#Budget.SetLimit}

```go
func (b *Budget) SetLimit(n int64)
```

SetLimit changes the budget's limit in bytes (0 for none), as when the process nears its memory limit. The zstd streams above their fair share under the new limit move down at their next write, or, when it leaves no room for them, send their frames uncompressed until it does. Brotli and gzip streams keep their tiers until they close.

#### func (*Budget) Stats {#Budget.Stats}

```go
func (b *Budget) Stats() BudgetStats
```

Stats reads the budget.

### type BudgetOptions {#BudgetOptions}

```go
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
```

BudgetOptions configure a Budget. The zero value takes half the process's memory limit and windows of 2^18 to 2^19.

### type BudgetStats {#BudgetStats}

```go
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
	ZstdWindow int
	// ZstdLevel is zstd's level (default zstd.SpeedFastest). A higher one
	// sends fewer bytes per frame for more CPU and heap per stream:
	// bench/RESULTS.md has the trade-off measured on a shared board.
	ZstdLevel zstd.EncoderLevel
	// ZstdLowMemory keeps zstd's history at its window: about 1 MiB per
	// stream instead of 1.65 MiB, for twice the CPU per frame once the
	// streams no longer fit in the CPU's cache (measured with 200 streams),
	// since the encoder then shifts its window after every 128 KiB of input.
	ZstdLowMemory bool
	// BrotliQuality is brotli's quality, 1 to 11 (default 4).
	BrotliQuality int
	// BrotliWindow is the base 2 logarithm of brotli's window (default 18).
	BrotliWindow int
	// NoCompression sends the stream as it is, whatever the client accepts:
	// for small, frequent events, where an encoder's heap (0.4 MiB and up
	// per stream) costs more than it saves.
	NoCompression bool
	// Budget, if set, is shared by streams: it picks each stream's window
	// within a memory limit. ZstdWindow and BrotliWindow are then the
	// largest a stream gets, ZstdLowMemory keeps it to low-memory windows,
	// and BrotliQuality is the quality of the larger brotli tiers (the
	// smallest are quality 3).
	Budget *Budget
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

Hibernate ends the stream's zstd frame and gives its encoder to a pool shared by streams, and its tier back to its budget, so that an idle stream holds neither. The next write takes an encoder from the pool and starts a new frame, without the history of the frames before; with a budget, of the tier the budget then allows, or in raw blocks while it allows none. A Content-Encoding: zstd body may hold any number of frames one after another, and browsers decode them as one stream. Hibernate does nothing to a stream of another coding: a brotli or gzip body is a single stream, which cannot start over without its history.

#### func (*Stream) Sent {#Stream.Sent}

```go
func (s *Stream) Sent() (events, wire int64)
```

Sent reports the bytes of events written to the stream and the bytes that went out for them, compressed.

#### func (*Stream) Tier {#Stream.Tier}

```go
func (s *Stream) Tier() (name string, heap int64)
```

Tier names what the stream's encoder holds now, as BudgetStats.Streams counts it: "zstd 2^19", "zstd 2^18 low memory", "br q4 2^18" or "gzip"; "zstd asleep" while a zstd stream has no encoder (it hibernates, or its budget has no room for it); "identity" when it goes out uncompressed. heap is what the stream's budget reserves for the encoder: 0 without a budget, and for a stream asleep or uncompressed.

#### func (*Stream) Write {#Stream.Write}

```go
func (s *Stream) Write(events ...[]byte) error
```

Write sends events: it compresses them together, flushes the encoder and writes the result to the client within the write timeout. A write the client does not take in time fails with an error that wraps os.ErrDeadlineExceeded, and one cut short by the end of the request's context with the context's cause. After a failed write the stream is broken, and every later write returns the same error.
