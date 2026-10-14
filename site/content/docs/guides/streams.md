---
title: Streams and compression
nav: Streams
headline: Streams: compression per stream within a budget
summary: Each stream compresses its frames with an encoder of its own. Pick the window by the largest frame, share memory through the budget, let idle streams hibernate, and leave small events uncompressed.
description: Each SSE stream compresses with its own encoder: pick the window by the largest frame, share memory through the budget, and let idle streams hibernate.
order: 6
---

Every tab holds one open stream, and every stream compresses what it sends with an encoder of its own. The encoder's window spans the whole stream: a frame that repeats most of the frame before refers back to it and costs a few hundred bytes, which is what makes rendering whole regions affordable ([Hypermedia](/docs/concepts/hypermedia)). The window is also memory, held for as long as the tab stays open.

## The encodings

`sse.Open`, which `hub.Serve` calls, picks the encoding from the request's `Accept-Encoding`: the one the browser rates highest, zstd before brotli before gzip when they tie.

| Encoding | Defaults | Heap per stream |
| --- | --- | --- |
| zstd | fastest level, window 2^19 (512 KiB), history twice the window | about 1.7 MiB |
| zstd, `ZstdLowMemory` | the same, history at the window | about 1.0 MiB |
| brotli | quality 4, window 2^18 (256 KiB) | about 1.6 MiB |
| gzip | fastest level | about 0.8 MiB |

Chrome, Firefox and Edge accept zstd; Safari decodes it from version 26.3, and older Safari gets brotli. On lith's shared board, zstd sends 2.6 times brotli's bytes for about half its CPU ([Performance](/docs/performance/encoders)). Datastar's Go SDK, given `WithBrotli` without `WithBrotliLGWin`, keeps brotli's 4 MiB window: 9.9 MiB of heap per stream, against 1.0 to 1.7 MiB for lith's zstd.

`sse.Options` sets the encoders, through `live.Options.SSE` for a hub's streams:

| `sse.Options` | Default | What it does |
| --- | --- | --- |
| `ZstdLevel` | `zstd.SpeedFastest` | `SpeedDefault` halves the bytes for no more CPU, at 1.1 MiB more per stream |
| `ZstdWindow` | 19 | zstd's window, as a base 2 logarithm; with a budget, the largest it gives |
| `ZstdLowMemory` | false | history at the window: 0.65 MiB less per stream, more CPU per frame |
| `BrotliQuality`, `BrotliWindow` | 4, 18 | brotli's quality and window; with a budget, the largest window it gives |
| `NoCompression` | false | every stream as it is ([below](#small-frequent-events)) |
| `Slow` | false | a denser encoder for a client on a slow link, while the budget has room ([below](#slow-links)) |
| `WriteTimeout` | 10 s | the deadline of each write |
| `Compress` | half the CPUs, in a hub | a semaphore that bounds how many streams compress at once |
| `Budget` | half the memory limit, in a hub | the memory the streams share, and the window each gets ([below](#the-memory-budget)) |

## Every write has a deadline

Each write to a stream gets its own deadline, `WriteTimeout`: a client that does not take a frame in time loses its stream, and nobody else waits for it. So the HTTP server runs without a `WriteTimeout` of its own, which would cut every stream after that long. A stream holds a slot of the `Compress` semaphore only while it compresses, never while its bytes go out on the network, so a few stalled clients cannot hold up the others.

## The window holds the largest frame

A frame can refer back only as far as the window reaches. When the window is smaller than the frame, the frame cannot find the previous one, and its bytes on the wire grow several times over. lith's benchmark board, a 142 KB frame, measured at zstd's fastest level:

| Window | Bytes per frame |
| --- | --- |
| 2^17 (128 KiB) | 10,831 |
| 2^18 (256 KiB) | 2,703 |
| 2^19 (512 KiB) | 1,471 |

A budget picks that window for each stream from its largest write ([below](#the-memory-budget)). Without a budget, or with one without a limit, measure the largest frame of each page, the full board, the longest list, and pick the smallest window that holds it with room to spare. The board app's largest frame, three lanes of 50 cards with long titles, is 51 KB: 2^16 holds it. A page that sends one view after another on the same stream refers back further than one frame: libretto's frames, 2 to 37 KB, cost 9% more bytes at 2^17 than at 2^19 and 19% more at 2^16. The [budget's table](/docs/performance/budget#settings) has every window for two workloads.

`Stream.Sent()` returns the bytes of events a stream was given and the bytes it sent, compressed. A test that renders the largest state of a page and checks the frame's length catches a page that outgrows its window.

## The memory budget

A hub shares a memory budget among its streams, so that the server holds what its memory allows, however many tabs are open. The budget books what each stream holds: its encoder, 32 KiB for the rest of it (the hub's state, the goroutines' stacks, net/http's buffers), and the newest frame of each view that two or more streams watch, once for all of them. A tab's own view books nothing: the hub drops its frame once the tab's stream has it, and keeps the frame's length and hash for the unchanged-frame check. The default needs no setting:

- The limit is half the process's memory limit: `GOMEMLIMIT`, else the cgroup's `memory.max` or `memory.high`, whichever is lower. The other half is for the rest of the app and for the garbage between two collections: a render of a 233 KB view allocates about 1 MiB. Without a memory limit the budget only counts, and every stream gets the largest window its options allow.
- A stream's first write picks its tier. The window must hold the stream's largest write with a sixteenth to spare, or a frame cannot refer to the one before: at 2^18, a 259 KB frame takes 249 bytes and a 266 KB one 29,719.
- A zstd stream takes the largest window that fits both its fair share (what the limit leaves after the frames and the streams, divided by the streams that compress) and what is left. When a stream finds no room, every zstd stream above the fair share of that moment moves to a smaller window at its next frame, down to the window its frame needs. That frame starts without history, once. A zstd stream without room sends raw blocks, and compresses again at a later frame once room frees up (with `Refuse`, a stream always has the window its frame needs, [below](#refusing-streams)).
- A brotli body is one stream with one window, so a brotli stream keeps its tier until it closes. It takes the window its frame needs and no larger one, which would send no fewer bytes: at quality 4 while the budget stays at most half full, then at quality 3, and only when that does not fit, quality 3 at 2^16 (720 KiB), which compresses each frame alone: 17 KB for a 235 KB board instead of 259 bytes. When not even that fits, it goes out uncompressed.
- A brotli stream that took 2^16 for a larger frame, or went out uncompressed, comes back on a better tier: once room frees up, the hub ends the stream with `sse.Reconnect` and the page opens it again, as after a deploy. It needs `live.Spec.Reconnect`. The budget asks one stream a second, and the hub the same tab once a minute.

| `sse.BudgetOptions` | Default | What it does |
| --- | --- | --- |
| `Limit` | half the memory limit | bytes the streams may hold together, encoders and frames; -1 for no limit |
| `MinWindow` | none | the smallest window a stream's frames need, for pages whose frames refer back further than one write |
| `MaxWindow` | 19 | the largest window a stream gets |
| `Refuse` | false | 503 for a stream the budget has no room for (32 KiB and the smallest tier whose window holds its frame, or 32 KiB when it compresses nothing), instead of no compression or a window below its frame |

A stream's own `ZstdWindow` and `BrotliWindow` cap what the budget gives it, and `ZstdLowMemory` keeps it to the low-memory tiers. `hub.Budget().Stats()` says what the streams hold now: the limit, the bytes reserved and the frames among them, the streams by tier (`zstd 2^19`, `zstd 2^18 low memory`, `br q4 2^18`, `br q3 2^16`, `identity`), the streams refused, and those asked to open again. `SetLimit` moves the limit while the hub runs, and the zstd streams above their new share move down at their next frame. Streams outside a hub share a budget by passing the same `*sse.Budget` in their `sse.Options`; a frame they keep for several streams, they book with `Budget.Hold`.

[The budget's measurements](/docs/performance/budget) show the policies side by side, up to 2,000 streams.

### Refusing streams

For pages with large, frequent frames, an uncompressed stream sends every frame whole: the board's 142 KB twenty times a second. Such an app refuses the streams that do not fit:

```go source=site/internal/docs/example/web/hub.go
SSE: sse.Options{Budget: sse.NewBudget(sse.BudgetOptions{Refuse: true})},
```

With `Refuse`, `sse.Open` returns `sse.ErrNoRoom` for every stream the budget has no room for: one that compresses needs 32 KiB and the smallest tier of its coding whose window holds its frame, one that compresses nothing (its browser accepts no coding, or `NoCompression` is set) its 32 KiB. The frame's length is `sse.Options.Frame`: `hub.Serve` waits for the first frames of the stream's views, a frame the hub keeps or one rendered in the next tick, and passes their length when it opens the stream. So the response's headers go out with the first frame, up to a tick later than they would without the wait, and the first frame comes as soon as before. A stream opened without `Frame` needs room for its coding's smallest tier.

A stream the budget lets in keeps a window that holds its frame until it closes: a zstd stream keeps that tier booked while it has no encoder, before its first frame and while it hibernates, and moves no lower; a brotli stream takes no window below its frame. When the budget turns a stream away, the zstd streams above their frame's window move down to it at their next write, until the stream would fit. A stream whose frames outgrow its window later keeps its tier and sends more bytes a frame, and is never cut off: a zstd stream moves up once room frees up, a brotli stream with `Spec.Reconnect` opens again. A refused stream gets a 503 from the hub, and a page that opens its stream with `retry: 'error'` asks again, with Datastar's backoff, until a stream closes and makes room. In 120 MiB, 134 zstd tabs with a 233 KB view each get in at 2^18, or 91 brotli tabs ([the budget's measurements](/docs/performance/budget#what-lith-took-from-it)).

So memory caps the streams: the budget books what each tab holds and turns away the tab it has no room for, whatever its page. Such an app leaves `live.Options.MaxStreams` high, at its default of 2000, as a backstop for a process without a memory limit, where the budget only counts, and every page opens its stream with `retry: 'error'`.

## Hibernation

Most tabs sit still: a page left open beside the one someone works in, a dashboard nobody looks at. (A tab in the background has no stream at all: Datastar closes a `@get` while its tab is hidden and opens it again when the tab shows.) With `live.Options.Hibernate` set, a zstd stream that sent no frame for that long ends its zstd frame and gives its encoder back to a pool, and its window back to the budget (with `Refuse`, all but the window its frame needs). Its next frame takes an encoder from the pool and starts without history. Heartbeats go out without waking it.

The cost is that first frame after a pause: in lith's simulation of 200 tabs over an hour, hibernating after 30 s cut the heap per tab from 0.64 to 0.11 MiB and added about 45 KB per tab and hour on the wire ([Performance](/docs/performance/hibernation)). Brotli and gzip streams cannot start over without their history, so hibernation applies to zstd only. It is off unless an app sets it; this site sets a minute.

## Small, frequent events

An encoder's heap, 0.4 MiB and more per stream, buys little when the events are small: a cursor's position, a counter. `sse.Options.NoCompression` sends every stream as it is, whatever the browser accepts. In a hub it applies to every stream, so an app with one such page and others with large frames gives that page a hub of its own.

## Slow links

A phone on a slow link pays for each frame twice: its bytes on the wire, and the work of its main thread to apply it. A stream that knows its link is slow takes fewer frames and a denser encoder.

**What the page measures.** The page measures once, as it starts, into a local signal, and opens its stream with `net=slow` when the link is slow; its own URL stays as it is. Where the browser has `navigator.connection` (Chromium), the link is slow when the user asked to save data or the browser calls it 2g or 3g. Firefox and Safari have no such API, and there Navigation Timing tells: the document's first byte more than 300 ms after its request, or the document coming at under 100 KB/s. The thresholds sit just past where `navigator.connection` turns from 4g to 3g, a round trip of 270 ms or 700 kbit/s. The template's page puts the measurement in its `data-signals` as `_slow`, and its stream's element reads it:

```go source=template/internal/views/expr.go
// slowLink is true on a slow link, measured once as the page starts. Where
// the browser has navigator.connection (Chromium): data saving, or a link
// it calls 2g or 3g. Elsewhere, from Navigation Timing: the document's
// first byte more than 300 ms after its request, or under 100 KB/s.
const slowLink = `((c, n) => c ? c.saveData || ['slow-2g', '2g', '3g'].includes(c.effectiveType) : !!n && n.transferSize > 0 && (n.responseStart - n.requestStart > 300 || n.transferSize < 100 * (n.responseEnd - n.responseStart)))(navigator.connection, performance.getEntriesByType('navigation')[0])`

// openStream opens the tab's stream on the element with id "stream", with
// net=slow on a slow link (edge.SlowLink). retry 'error' retries through
// network errors, 502s and 503s and never gives up, but leaves a stream
// the server ends closed: on a deploy the server ends it with
// sse.Reconnect, which runs this again.
const openStream = `@get('/stream?tab=' + $_tab + ($_slow ? '&net=slow' : ''), {retry: 'error', retryMaxCount: Infinity, retryMaxWait: 10000})`
```

The measurement is a Datastar expression, so it needs no script beyond the CSP's `'unsafe-eval'`, which Datastar needs anyway. It runs in `data-signals`, not in the stream's `data-init`, which runs again on every reconnect.

**What the server reads.** `edge.SlowLink(r)` reports a slow link for `net=slow`, for the `Save-Data: on` header, and for Chromium's client hints: `ECT` of slow-2g, 2g or 3g, `RTT` over 300 ms, `Downlink` under 0.8 Mbit/s. Chromium sends the hints with every request once a document has asked for them with `Accept-CH: ECT, RTT, Downlink` (`edge.NetworkHints`), the stream's request included, so they cover a page whose own measurement missed. The template's documents send that header, and its stream handler passes the answer on:

```go source=template/internal/web/server.go
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	if !validID(tab) {
		http.Error(w, "bad tab id", http.StatusBadRequest)
		return
	}
	sid := edge.SessionID(r.Context())
	spec := live.Spec{ID: tab, Views: []live.View{entriesView(), tabView(sid, tab)}, Reconnect: "stream", Slow: edge.SlowLink(r)}
	if err := s.hub.Serve(w, r, spec); err != nil {
		s.log.Debug("stream ended", "tab", tab, "err", err)
	}
}
```

**What a slow stream gets.** With `live.Spec.Slow`:

- Each view's newest frame, 250 ms or more after the last: about four frames a second. The frames between drop, as they do for a stream that falls behind. A view's first frame goes out at once, and so does a frame of a view that sent nothing for 250 ms, such as the tab's outcome after an action; it takes the frames that wait along, so views that change together still arrive together. Heartbeats go out as before.
- `sse.Options.Slow`, which asks the budget for a denser encoder while it has room: zstd at its default level instead of its fastest, while that tier, about 1.1 MiB more, fits the stream's fair share and what is left, and the fastest level's when it does not; brotli at quality 4 while the budget has room for it, where another stream drops to 3 once the budget is half full. The default level sends half the bytes of the fastest on frames that change in many places and 4 to 8% fewer where a frame changes in one place, and nothing on a Game of Life ([Encoders](/docs/performance/encoders#for-slow-links)).

A client that claims a slow link without one gets fewer frames and costs some memory, within the budget; a slow link the page takes for a fast one gets what every stream gets.

On this site, every demo's stream and the landing page's pass `edge.SlowLink(r)`. Measured on 2026-10-14 on the development machine, in headless Chromium 154 with DevTools' network presets and its CPU four times slower, as on a phone (`TestSlowLinkCost` in `site/internal/app`), with this commit and with the site and lith as of v0.2.0, three runs each, the medians over 10 s with the range in brackets: the stream's KB/s and frames a second, the main thread's share of the time in tasks, and the longest wait of a key press, sent every half second. The Game of Life steps ten times a second; the landing page has five cursors moving behind its title. Unthrottled, both builds send the Game of Life 5.0 to 7.2 KB/s, ten frames a second, and keep the main thread 35 to 43% busy.

| Page, link | Stream | KB/s | Frames a second | Main thread busy | Slowest key |
| --- | --- | --- | --- | --- | --- |
| Game of Life, Slow 4G | v0.2.0 | 5.2 (4.4 to 6.6) | 10 | 89% (83 to 92) | 68 ms |
| | slow | 2.9 (2.3 to 3.2) | 4 | 54% (49 to 61) | 67 ms |
| Game of Life, 3G | v0.2.0 | 6.2 (5.5 to 6.3) | 10 | 90% (84 to 96) | 71 ms |
| | slow | 2.7 (2.5 to 3.4) | 4 | 56% (53 to 58) | 52 ms |
| Landing page, Slow 4G | v0.2.0 | 0.9 | 17 | 75% (64 to 79) | 30 ms |
| | slow | 0.3 | 6.8 (6.7 to 7.0) | 51% (47 to 52) | 18 ms |
| Landing page, 3G | v0.2.0 | 0.9 | 17 | 64% (63 to 67) | 15 ms |
| | slow | 0.3 | 6.8 (6.7 to 6.9) | 49% (45 to 50) | 18 ms |

- The network was not the limit: Slow 4G carries 180 KB/s and 3G 50 KB/s, and the streams took at most 7 KB/s. What a slow phone loses on is its main thread, and fewer frames cut its busy time by a quarter to two fifths.
- The streams send half the bytes or less: fewer frames, each a few generations or moves apart from the one before, so each is larger (the Game of Life's about 700 bytes instead of 520).
- Every page stayed interactive: no key press waited more than 71 ms from DevTools sending it to the page taking it, and Chromium's Event Timing measured none over 104 ms, under the 200 ms that counts as a good response.
- The landing page sends more than four frames a second: its counter and the stream inspector's report change once a second, are due at once, and take the cursors' newest frame along.

## Streams without the hub

A handler that streams without the hub opens an `sse.Stream` itself: `sse.Open(w, r, opts)`, then `st.Write(events...)` for each frame and `st.Close()` before it returns. Events are formatted once with `sse.PatchElements`, `sse.PatchSignals` and `sse.Comment`, and the same bytes can go to any number of streams. A stream's methods are not safe for concurrent use: one goroutine writes. When the server stops, the handler ends with `st.CloseWith(sse.Reconnect(id))`, as the hub does, so the page opens the stream again on the next process.
