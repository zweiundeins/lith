---
title: Streams and compression
nav: Streams
headline: Streams: compression per stream within a budget
summary: Each stream compresses its frames with an encoder of its own. Pick the window by the largest frame, share memory through the budget, let idle streams hibernate, and leave small events uncompressed.
description: Each SSE stream compresses with its own encoder: pick the window by the largest frame, share memory through the budget, and let idle streams hibernate.
order: 7
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

A budget picks that window for each stream from its largest write ([below](#the-memory-budget)). Without a budget, or with one without a limit, measure the largest frame of each page, the full board, the longest list, and pick the smallest window that holds it with room to spare. The board app's largest frame, three lanes of 50 cards with long titles, is 51 KB: 2^16 holds it. A page that sends one view after another on the same stream refers back further than one frame: a business app's frames, 2 to 37 KB, cost 9% more bytes at 2^17 than at 2^19 and 19% more at 2^16. The [budget's table](/docs/performance/budget#settings) has every window for two workloads.

`Stream.Sent()` returns the bytes of events a stream was given and the bytes it sent, compressed. A test that renders the largest state of a page and checks the frame's length catches a page that outgrows its window.

## The memory budget

A hub shares a memory budget among its streams, so that the server holds what its memory allows, however many tabs are open. The budget books what each stream holds: its encoder, 32 KiB for the rest of it (the hub's state, the goroutines' stacks, net/http's buffers), and the newest frame of each view that two or more streams watch, once for all of them. A tab's own view books nothing: the hub drops its frame once the tab's stream has it, and keeps the frame's length and hash for the unchanged-frame check. The default needs no setting:

- The limit is half the process's memory limit: `GOMEMLIMIT`, else the cgroup's `memory.max` or `memory.high`, whichever is lower. The other half is for the rest of the app and for the garbage between two collections: a render of a 233 KB view allocates about 1 MiB. A process with neither takes half the machine's memory as its limit (Linux's `MemTotal`), so the streams get a quarter of it, and `live.New` logs a warning: without any limit every zstd stream would hold the largest tier, about 1.7 MiB, and the 2,000 streams `MaxStreams` allows 3.3 GiB. Where the machine's memory is not known either, the budget only counts, and every stream gets the largest window its options allow. `live.New` logs the limit and where it came from (`BudgetStats.From`).
- A stream's first write picks its tier. The window must hold the stream's largest write with a sixteenth to spare, or a frame cannot refer to the one before: at 2^18, a 259 KB frame takes 249 bytes and a 266 KB one 29,719.
- A zstd stream takes the largest window that fits both its fair share (what the limit leaves after the frames and the streams, divided by the streams that compress) and what is left. When a stream finds no room, every zstd stream above the fair share of that moment moves to a smaller window at its next frame, down to the window its frame needs. That frame starts without history, once. A zstd stream without room sends raw blocks, and compresses again at a later frame once room frees up (with `Refuse`, a stream always has the window its frame needs, [below](#refusing-streams)).
- A brotli body is one stream with one window, so a brotli stream keeps its tier until it closes. It takes the window its frame needs and no larger one, which would send no fewer bytes: at quality 4 while the budget stays at most half full, then at quality 3, and only when that does not fit, quality 3 at 2^16 (720 KiB), which compresses each frame alone: 17 KB for a 235 KB board instead of 259 bytes. When not even that fits, it goes out uncompressed.
- A brotli stream that took 2^16 for a larger frame, or went out uncompressed, comes back on a better tier: once room frees up, the hub ends the stream with `sse.Reconnect` and the page opens it again, as after a deploy. It needs `live.Spec.Reconnect`. The budget asks one stream a second, and the hub the same tab once a minute.

| `sse.BudgetOptions` | Default | What it does |
| --- | --- | --- |
| `Limit` | half the memory limit, or a quarter of the machine's memory | bytes the streams may hold together, encoders and frames; -1 for no limit |
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

So memory caps the streams: the budget books what each tab holds and turns away the tab it has no room for, whatever its page. Such an app leaves `live.Options.MaxStreams` high, at its default of 2000, as a backstop for a machine whose memory the budget does not know, where it only counts, and every page opens its stream with `retry: 'error'`.

### One client, many streams

`MaxStreams` and the budget count streams, not clients. An idle stream costs its client nothing, so one address that opens 200 of them gives every other page a 503 on its stream; and since the budget shares its room per stream and never takes a tier back, the streams that came first keep the compressed tiers while a page that comes later gets every frame whole, or, with `Refuse`, a 503. A stream handler names the client in `live.Spec.Client`, and the hub caps each client at `Options.MaxStreamsPerClient` streams, 16 by default, with a 429 (`live.ErrClientFull`):

```go source=template/internal/web/server.go
	spec := live.Spec{
		ID:        streamID(sid, tab),
		Client:    edge.ClientKey(r),
		Views:     []live.View{entriesView(), tabView(sid, tab)},
		Reconnect: "stream",
		Slow:      edge.SlowLink(r),
		Ack:       ackEvent(tab),
		Build:     page.Build,
		Page:      r.Header.Get("Page-Path"),
		Dirty:     page.Dirty,
		Refresh: func(ctx context.Context, rq live.RefreshRequest) ([][]byte, error) {
			return s.refresh(ctx, rq, page.Assets)
		},
	}
```

The key is the client's address (`edge.ClientKey`, an IPv6 client by its /64), not the session, which a client mints anew with every request that sends no cookie. A browser holds one stream per tab: 16 leaves room for a person with many tabs, or a few people behind one address, while it takes an attacker MaxStreams/16 addresses to fill the hub, 13 for the template's 200. An app whose users share one address, an office or a VPN, keys a signed-in user by the account instead, or raises the cap; a negative cap turns it off, and a stream without a `Client` counts under none. In `TestLateClientKeepsCompression`, with a budget for four streams at the window a 230 KB view needs, a client that opened ten streams first left a later client 230,639 bytes for each change of the view; capped at three, the later client kept its window and sent about 4 KB.

`Watch` keeps a view for a stream ID that is not open yet for 10 s, for at most `MaxStreams` IDs. The cap per client does not bound them, and need not: an app calls `Watch` with IDs it checked, such as a stream named after the request's session (the template's `streamID`), or from an action that refuses a tab without an open stream (this site's demos), so a client cannot fill it with made-up IDs.

## Hibernation

Most tabs sit still: a page left open beside the one someone works in, a dashboard nobody looks at. (A tab in the background has no stream at all: Datastar closes a `@get` while its tab is hidden and opens it again when the tab shows.) With `live.Options.Hibernate` set, a zstd stream that sent no frame for that long ends its zstd frame and gives its encoder back to a pool, and its window back to the budget (with `Refuse`, all but the window its frame needs). Its next frame takes an encoder from the pool and starts without history. Heartbeats go out without waking it.

The cost is that first frame after a pause: in lith's simulation of 200 tabs over an hour, hibernating after 30 s cut the heap per tab from 0.64 to 0.11 MiB and added about 45 KB per tab and hour on the wire ([Performance](/docs/performance/hibernation)). Brotli and gzip streams cannot start over without their history, so hibernation applies to zstd only. It is off unless an app sets it; this site sets a minute.

## Small, frequent events

An encoder's heap, 0.4 MiB and more per stream, buys little when the events are small: a cursor's position, a counter. `sse.Options.NoCompression` sends every stream as it is, whatever the browser accepts. In a hub it applies to every stream, so an app with one such page and others with large frames gives that page a hub of its own.

## Slow links

A phone pays for each frame twice: its bytes on the wire, and the work of its main thread to apply it. A stream that knows its client is slow takes fewer frames.

What a frame costs the page is mostly the number of its elements and their layout and paint, not its bytes, and not the morph alone. Measured on 2026-10-17 on the development machine with `TestLifeDrift` in headless Chromium 154 behind Caddy with HTTP/2 (`bench/frame-cost/RESULTS.md`): at 15 times CPU the site's Game of Life frame costs about 440 ms of the main thread when it is rendered, of which the morph is 130 ms; layout (94.4 ms), painting 4,000 gradient backgrounds (80.9 ms), the population sparkline (50.2 ms) and Datastar's MutationObserver (32.9 ms) take most of the rest. The board drawn with the least markup, 4,000 `<p>` without rows or ids, halves the HTML (20,623 bytes against 46,450) but keeps the 4,000 elements, sends 1,810 bytes a frame on the wire against 709, and is already behind at 12 times, where the site's breaks at 10. The board as one `<svg>` with a path per colour, 34 elements, keeps up without any fix at 15 times (lag 0.00 s, three runs of 150 s) and applies every frame up to 120 times. A terminal of 80 by 24 with one span per character, every cell new in every frame, breaks at 15 times; with one span per run of the same colours, at 50 times; where either breaks, `newest-frame.js` with `__reopen` and pacing by the page both bring it back within a second. A frame sent as a signal patch, such as the board drawn into a `<canvas>` by a `data-effect`, is out of `newest-frame.js`'s reach, since the plugin never drops a signal patch: only `__reopen` or pacing help such a page.

**What the page measures as it starts.** The page measures its link once, as it starts, into a local signal, and its stream's request sends `Save-Data: on` when the link is slow; the stream's URL and the page's own stay as they are. Where the browser has `navigator.connection` (Chromium), the link is slow when the user asked to save data or the browser calls it 2g or 3g. Firefox and Safari have no such API, and there Navigation Timing tells: the document's first byte more than 300 ms after its request, or the document coming at under 100 KB/s. The thresholds sit just past where `navigator.connection` turns from 4g to 3g, a round trip of 270 ms or 700 kbit/s. The template's page puts the measurement in its `data-signals` as `_slow`, and its stream's element reads it:

```go source=template/internal/views/expr.go
// slowLink is true on a slow link, measured once as the page starts. Where
// the browser has navigator.connection (Chromium): data saving, or a link
// it calls 2g or 3g. Elsewhere, from Navigation Timing: the document's
// first byte more than 300 ms after its request, or under 100 KB/s.
const slowLink = `((c, n) => c ? c.saveData || ['slow-2g', '2g', '3g'].includes(c.effectiveType) : !!n && n.transferSize > 0 && (n.responseStart - n.requestStart > 300 || n.transferSize < 100 * (n.responseEnd - n.responseStart)))(navigator.connection, performance.getEntriesByType('navigation')[0])`

// openStream opens the tab's stream on the element with id "stream". On a
// slow link, measured as the page starts, it sends Save-Data: on
// (edge.SlowLink). Datastar cancels by the URL written here, without the
// signals, so a stream opened again aborts the old request. retry 'error'
// retries network errors and every answer from 400 to 599 and never gives
// up, which carries the page through a restart; a 204 or a 3xx stops it,
// and a stream the server ends stays closed: on a deploy the server ends
// it with sse.Reconnect, which runs this again. It marks the page out of
// sync until the tab view's first frame. For a refresh after a deploy
// (web.refresh) it reports the page's build, assets version and whether
// the person typed as signals, which Datastar reads afresh for every
// reopen, a retry or the tab shown again, where it repeats the first
// request's headers; the path, which never changes, goes in a header.
const openStream = `($_synced = false, @get('/stream?tab=' + $_tab, {headers: {'Page-Path': location.pathname + location.search, ...($_slow ? {'Save-Data': 'on'} : {})}, filterSignals: {include: /^_(build|assets|dirty)$/, exclude: /(?!)/}, retry: 'error', retryMaxCount: Infinity, retryMaxWait: 10000}))`
```

The measurement is a Datastar expression, so it needs no script beyond the CSP's `'unsafe-eval'`, which Datastar needs anyway. It runs in `data-signals`, not in the stream's `data-init`, which runs again on every reconnect.

**What the server reads.** `edge.SlowLink(r)` reports a slow link for the `Save-Data: on` header, which browsers also send when their user asked to save data, for Chromium's client hints: `ECT` of slow-2g, 2g or 3g, `RTT` over 300 ms, `Downlink` under 0.8 Mbit/s, and for the query parameter `net=slow`, which pages put in their stream's URL before they sent `Save-Data`, so that tabs still open from then keep their slow stream. Chromium sends the hints with every request once a document has asked for them with `Accept-CH: ECT, RTT, Downlink` (`edge.NetworkHints`), the stream's request included, so they cover a page whose own measurement missed. The template's documents send that header, and its stream handler passes the answer on:

```go source=template/internal/web/server.go
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	var page pageState
	if !validID(tab) || sse.ReadSignals(w, r, &page) != nil {
		w.WriteHeader(http.StatusBadRequest) // no page of ours sends it
		return
	}
	sid := edge.SessionID(r.Context())
	spec := live.Spec{
		ID:        streamID(sid, tab),
		Client:    edge.ClientKey(r),
		Views:     []live.View{entriesView(), tabView(sid, tab)},
		Reconnect: "stream",
		Slow:      edge.SlowLink(r),
		Ack:       ackEvent(tab),
		Build:     page.Build,
		Page:      r.Header.Get("Page-Path"),
		Dirty:     page.Dirty,
		Refresh: func(ctx context.Context, rq live.RefreshRequest) ([][]byte, error) {
			return s.refresh(ctx, rq, page.Assets)
		},
	}
	if err := s.hub.Serve(w, r, spec); err != nil {
		s.log.Debug("stream ended", "tab", tab, "err", err)
	}
}
```

**What a slow stream gets.** With `live.Spec.Slow`, each view's newest frame, 250 ms or more after the last: about four frames a second. A view's first frame goes out at once, and so does a frame of a view that sent nothing for 250 ms, such as the tab's outcome after an action; it takes the frames that wait along, so views that change together still arrive together. Heartbeats go out as before. While a frame waits for its turn, the hub does not render its view again unless another stream keeps up with it ([Ticks](/docs/concepts/ticks#the-newest-frame-wins)): a tab's own view renders about four times a second instead of at every tick, and its frame can be up to 250 ms older than the view when it goes out. A view that faster streams watch too renders for them, and the slow stream's frames between drop.

A client that claims a slow link without one gets fewer frames and costs the server less; a slow link the page takes for a fast one gets what every stream gets.

On this site, every demo's stream and the landing page's pass `edge.SlowLink(r)`. Measured on 2026-10-14 on the development machine, in headless Chromium 154 with DevTools' network presets and its CPU four times slower, as on a phone (`TestSlowLinkCost` in `site/internal/app`), with the commit that added slow streams, which then also took a denser zstd level, and with the site and lith as of v0.2.0, three runs each, the medians over 10 s with the range in brackets: the stream's KB/s and frames a second, the main thread's share of the time in tasks, and the longest wait of a key press, sent every half second. The Game of Life steps ten times a second; the landing page has five cursors moving behind its title. Unthrottled, both builds send the Game of Life 5.0 to 7.2 KB/s, ten frames a second, and keep the main thread 35 to 43% busy.

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
- Measured again on 2026-10-15, one run, with slow streams as they are now, without the denser level and with the render skip, the slow rows came out within those ranges or next to them: the Game of Life 2.8 and 3.2 KB/s with its main thread 52 and 53% busy, the landing page 0.4 KB/s with 46 and 50%.

**A slow phone on a fast network.** The server cannot tell such a phone by its writes. A frame of the Game of Life is about 700 bytes on the wire, and the buffers between the server and the page hold minutes of them, so a page that applies frames more slowly than they come falls further and further behind, and the server's writes never wait. Measured on 2026-10-15 on the development machine with the first version of `TestLifeDrift` (`site/internal/app`), which read the page late (see below): headless Chromium 154 on an unthrottled link with its CPU slowed 15 times, the least of 4, 6, 10, 15 and 20 at which the page could not keep up: it applied 7 of the board's 10 frames a second, where at 10 times it applied all ten and stayed 2 to 3 s behind. The board got a fresh soup every 15 s, so it stayed as busy as a new one. Every second the test compared the board's generation with the one the page showed, for 5 minutes, behind Caddy 2.10.2 with TLS and HTTP/2, as this site runs, and without Caddy, with `navigator.connection` saying 4g, and saying 3g, which has the page ask for a slow stream (then with `net=slow`), all without the plugin below:

| Behind the board after | Caddy, 4g | Caddy, 3g: slow stream | Without Caddy, 4g | Without Caddy, 3g: slow stream |
| --- | --- | --- | --- | --- |
| 30 s | 13.7 s (138 generations) | 0.7 s (7) | 13.3 s (133) | 0.9 s (10) |
| 1 minute | 23.1 s (232) | 0.9 s (10) | 22.5 s (225) | 0.6 s (7) |
| 2 minutes | 42.6 s (427) | 0.6 s (7) | 40.3 s (404) | 0.4 s (5) |
| 3 minutes | 60.5 s (605) | 1.1 s (11) | 58.9 s (589) | 0.4 s (5) |
| 4 minutes | 77.7 s (778) | 0.6 s (7) | 75.3 s (754) | 1.0 s (10) |
| 5 minutes | 98.1 s (981) | 0.2 s (3) | 94.1 s (941) | 0.4 s (5) |
| Frames dropped in 5 minutes | 0 | 1,829 | 0 | 1,828 |
| Longest write of the site | 0.16 ms | 0.13 ms | 0.12 ms | 0.14 ms |

- How far behind is how long ago the board showed the generation the page showed. Without a slow stream the page applied about 7 frames a second and fell 3 generations further behind every second, for the whole five minutes, with Caddy and without it. The hub dropped no frame, and no write of the site's waited.
- The frames waited where the server cannot see them. Behind Caddy, Chromium took the stream as it came over HTTP/2 and held what the page had not applied, and the sockets' kernel buffers stayed empty: Chromium grants an HTTP/2 stream a window of 6 MiB, which this backlog, about 2 KB a second, would fill in about 50 minutes, before Caddy and the socket to the site hold more. Without Caddy, over HTTP/1.1, Chromium stopped reading the socket, and the frames waited in the kernel: the browser's receive buffer filled at about 110 KB, and the site's send buffer took the rest, up to 528 KB and still growing, which it may until `tcp_wmem`'s maximum, 16 MiB on the development machine and 4 MiB on this site's server.
- With a slow stream the page applied the four frames a second it got and stayed 0.6 s behind at the median, 1.2 s at most. The frames dropped are the slow stream's frames between its four a second, which the hub then still rendered; it now defers those renders instead.
- The measurement as the page starts looks at the link, not the CPU: on a fast link, a phone this slow opens a fast stream and falls behind as in the first column, unless the page watches its own lag.

That first version of the test asked the page for its generation through DevTools, which a page answers only when its main thread is free: without the plugin, right after a task of up to 7 s. The lags in the table were taken at those moments, and its frames a second counted per reading, not per second. The test now has the page note the time of every frame it applies and takes the lag from those notes. Measured with it on 2026-10-16, the same page with its CPU slowed 15 times behind Caddy with HTTP/2, 150 s, three runs: 27.15 s behind the board at the median (25.90 to 27.77), 50.0 s at the end, with 6.91 frames applied a second. Two runs of 120 s each ended 43.8 s behind with Caddy and 42.8 s behind over HTTP/1.1 without it (`bench/drift/RESULTS.md`). What the page applied is not what it showed: it painted about every 7 s, so what a user saw was up to 7 s older still.

Where a frame goes on its way, and where each fix below acts:

<figure class="chart diagram">
<figcaption class="chart__bar"><span class="chart__title">A frame's way from the hub to a page behind its stream</span><a class="chart__source" href="https://github.com/zweiundeins/lith/blob/main/bench/drift/RESULTS.md">bench/drift/RESULTS.md</a></figcaption>
<div class="chart__plot diagram__plot">
<svg viewBox="0 0 960 350" role="img" aria-labelledby="frame-way-title frame-way-note">
<title id="frame-way-title">A frame's way: hub, SSE write, Caddy, Chromium's network process where the frames wait, the pipe to the page, and the page's main thread, with the acknowledgement going back to the hub</title>
<defs><marker id="frame-way-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path class="diagram__head" d="M0,0 L10,5 L0,10 z"/></marker></defs>
<text class="diagram__wait" x="556" y="30" text-anchor="middle">frames wait here</text>
<g class="diagram__stage"><rect x="20" y="40" width="136" height="124" rx="4"/><text x="30" y="62"><tspan class="diagram__name">1 Hub</tspan><tspan x="30" dy="20">renders each view</tspan><tspan x="30" dy="16">once per tick,</tspan><tspan x="30" dy="16">one slot per view</tspan></text></g>
<g class="diagram__stage"><rect x="176" y="40" width="136" height="124" rx="4"/><text x="186" y="62"><tspan class="diagram__name">2 SSE write</tspan><tspan x="186" dy="20">about 700 B,</tspan><tspan x="186" dy="16">compressed;</tspan><tspan x="186" dy="16">never blocks</tspan></text></g>
<g class="diagram__stage"><rect x="332" y="40" width="136" height="124" rx="4"/><text x="342" y="62"><tspan class="diagram__name">3 Caddy</tspan><tspan x="342" dy="20">TLS, HTTP/2</tspan></text></g>
<g class="diagram__stage diagram__stage--wait"><rect x="488" y="40" width="136" height="124" rx="4"/><text x="498" y="62"><tspan class="diagram__name">4 Network process</tspan><tspan x="498" dy="20">HTTP/2 window</tspan><tspan x="498" dy="16">6 MiB a stream,</tspan><tspan x="498" dy="16">about 50 minutes;</tspan><tspan x="498" dy="16">HTTP/1.1: kernel</tspan><tspan x="498" dy="16">buffers</tspan></text></g>
<g class="diagram__stage"><rect x="644" y="40" width="136" height="124" rx="4"/><text x="654" y="62"><tspan class="diagram__name">5 Pipe to page</tspan><tspan x="654" dy="20">2 MiB a read,</tspan><tspan x="654" dy="16">about 45 frames</tspan></text></g>
<g class="diagram__stage"><rect x="800" y="40" width="136" height="124" rx="4"/><text x="810" y="62"><tspan class="diagram__name">6 Main thread</tspan><tspan x="810" dy="20">parse, then morph</tspan><tspan x="810" dy="16">all in one task,</tspan><tspan x="810" dy="16">145 ms a frame</tspan><tspan x="810" dy="16">at 15x; no paint</tspan><tspan x="810" dy="16">for 7.2 s</tspan></text></g>
<line class="diagram__flow" x1="156" y1="102" x2="174" y2="102" marker-end="url(#frame-way-arrow)"/>
<line class="diagram__flow" x1="312" y1="102" x2="330" y2="102" marker-end="url(#frame-way-arrow)"/>
<line class="diagram__flow" x1="468" y1="102" x2="486" y2="102" marker-end="url(#frame-way-arrow)"/>
<line class="diagram__flow" x1="624" y1="102" x2="642" y2="102" marker-end="url(#frame-way-arrow)"/>
<line class="diagram__flow" x1="780" y1="102" x2="798" y2="102" marker-end="url(#frame-way-arrow)"/>
<line class="diagram__link" x1="88" y1="164" x2="88" y2="200"/>
<g class="diagram__fix"><rect x="20" y="200" width="330" height="54" rx="4"/><text x="30" y="222"><tspan class="diagram__name">Ack pacing, hybrid</tspan><tspan x="30" dy="18">hold the next frame until the page acks</tspan></text></g>
<line class="diagram__link" x1="868" y1="164" x2="868" y2="200"/>
<g class="diagram__fix"><rect x="586" y="200" width="350" height="54" rx="4"/><text x="596" y="222"><tspan class="diagram__name">newest-frame.js, between parse and morph</tspan><tspan x="596" dy="18">newest frame of each view, pause after a batch</tspan></text></g>
<path class="diagram__back" d="M936,140 H950 V290 H8 V140 H18" marker-end="url(#frame-way-arrow)"/>
<text class="diagram__label" x="480" y="310" text-anchor="middle">back to the hub: POST /ack from data-init after the morph,</text>
<text class="diagram__label" x="480" y="328" text-anchor="middle">POST /pace when behind, or __reopen with Save-Data: on</text>
</svg>
</div>
<p class="diagram__note" id="frame-way-note">Chromium with its CPU slowed 15 times, the Game of Life behind Caddy, from performance traces of 20 s (2026-10-16). The hub keeps one frame per view for each stream and the server's writes never wait, so a page too slow for its stream does not show on the server: the frames wait in Chromium's network process, which an HTTP/2 stream's window of 6 MiB lets hold about 50 minutes of this backlog, or over HTTP/1.1 in the kernel's socket buffers. Once the page is behind, each read hands it 2 MiB, about 45 frames, and Datastar morphs them all in one task of about 7.2 s without a paint. Firefox keeps the frames in its parent process and hands the page one per task, so it keeps painting, but falls behind just as far. Ack pacing and the hybrid act at the hub, newest-frame.js in the page; the arrow back is how the page tells the server.</p>
</figure>

**What the page measures later: its own lag.** The browser holds the frames a page has not applied, so only the page can tell that it fell behind. Once it is behind, Chromium hands it 2 MiB of the stream per read, the size of the pipe between its network process and the page, about 45 frames of the Game of Life, and Datastar applies them all in one task, without a paint between ([Datastar notes](/docs/datastar#a-page-that-falls-behind-its-stream-stops-painting)). Firefox keeps the frames in its parent process and hands the page one per task; it never makes the server's writes wait, so a slow Firefox tab never shows on the server at all. lith ships a Datastar plugin for this, `newest-frame.js` (`live.NewestFrame`), the build of [datastar-attribute-newest-frame](https://github.com/mbolli/datastar-attribute-newest-frame) that lith vendors; that repository has its source, its tests and a live demo. With `data-newest-frame` on `<body>`, the page holds its stream's patch events and applies only the newest frame of each view among those that arrived together. After a batch it waits half as long as applying the batch took, so applying frames takes at most two thirds of the main thread, and Firefox gets the time to hand over what it holds. The rendering that each applied frame causes runs in that pause and is not counted: on a phone 15 times slower than a desktop, the Game of Life's main thread stayed 97% busy, half applying frames and half laying out and painting them, but it painted every frame it applied, about two a second, and stayed within a second of the board (performance traces of 20 s, 2026-10-16, `bench/drift/RESULTS.md`). Signal patches, appends, removes, scripts and view transitions are never dropped, and the order is kept. A page that keeps up applies every frame, as it arrives.

When the page drops more than half of its frames over 3 s, at least 10, the plugin dispatches `newest-frame-behind` on the stream's element, with the frames and the skipped frames of those 3 s in its detail, and goes on watching: while the page stays behind, it reports again every 3 s. The event bubbles up to `<body>`, where the template posts it to `/pace`, which counts it ([below](#counting-the-pages-that-fall-behind)) and has the server pace the stream ([Pacing a stream by the page](#pacing-a-stream-by-the-page)). The page keeps its stream, and drops what it cannot apply until the pacing takes over.

The template serves the plugin with its other static files, by its content's hash and as immutable:

```go source=template/internal/web/server.go
	// lith's newest-frame.js, which the page loads after Datastar.
	js, err := fs.ReadFile(live.Scripts, live.NewestFrame)
	if err != nil {
		return nil, err
	}
	static.Add("lith/"+live.NewestFrame, js)
```

The page loads it after Datastar, through an import map that maps `'datastar'` to Datastar's URL, since the plugin imports from it, and turns it on on its body, where it also listens for the report:

```templ source=template/internal/views/guestbook.templ
			@importMap(p)
			<script type="module" src={ p.JS }></script>
			<script type="module" src={ p.NewestFrame }></script>
		</head>
		<body data-newest-frame data-on:newest-frame-behind={ behind }>
```

The import map is an inline script, so it carries the request's CSP nonce (`edge.Nonce`). lith vendors the plugin's minified build, 3.0 KB and 1.5 KB with brotli, which `edge.Minify(minify.Bytes)` leaves as it is.

Measured on 2026-10-16 on the development machine with the plugin in this site's pages, with the corrected `TestLifeDrift`: headless Chromium 154 with its CPU slowed 15 times, on an unthrottled link behind Caddy with HTTP/2, 150 s, three runs. With the plugin alone, without pacing, the page stayed 0.32 s behind the board at the median and 0.94 s at most, and the site wrote 7.09 KB/s; without the plugin, 27.15 s at the median and 50.0 s at the end. At 6 and 8 times, where a page without the plugin still applies every frame but paints fewer, it painted 7.88 and 5.69 frames a second and stayed 0.06 and 0.12 s behind (`bench/gaps/RESULTS.md`, 2026-10-17).

**Opening a slow stream instead.** With `data-newest-frame__reopen`, the plugin also sets `$_slow` when it reports, and opens the stream again from a copy of its element. The new request sends `Save-Data: on`, and the server answers with a slow stream at the same URL. Datastar aborts the request in flight only when the new one goes to the same URL: a stream opened at another URL leaves the old one open, and the frames the browser holds of it keep arriving. The plugin asks once, and a page that measured a slow link as it started never asks. It is not the default, because the slow stream's four frames a second are fixed and the page never switches back. At 6 times CPU it capped the page at 4.03 frames a second, 0.37 s behind, where the page without any fix was 0.10 s behind; a page slowed for 60 s and then fast stayed on the slow stream at 4.02 frames a second for good (three runs each, `bench/gaps/RESULTS.md`). At 15 times it halves the bytes and doubles the lag: 0.64 s behind at the median (0.63 to 0.66), 1.16 s at most, at 3.20 KB/s. Firefox has no CPU throttling, so `TestNewestFrameSlowStream` makes every frame the page applies cost its main thread 300 ms more, alike in both engines: headless Chromium 154 and Firefox 158 each asked for a slow stream 2.8 s after the stream opened, and were 0.77 s and 0.61 s behind the board 4 s later. At full speed both applied every frame (`TestNewestFrame`).

## Counting the pages that fall behind

The server cannot see that a page falls behind, but the page can tell it, and the counts say whether the app's real users fall behind at all. The template posts each `newest-frame-behind` to `/pace` with its tab id, and the handler passes it to `live.Hub.Behind`, which counts it, before it paces the stream ([below](#pacing-a-stream-by-the-page)):

```go source=template/internal/web/pace.go
func (s *Server) pace(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	if !validID(tab) {
		w.WriteHeader(http.StatusBadRequest) // no page of ours sends it
		return
	}
	sid, now := edge.SessionID(r.Context()), time.Now()
	if s.paceAddrs.Allow(edge.ClientKey(r), 1, now) && s.paces.Allow(sid, 1, now) {
		id := streamID(sid, tab)
		s.hub.Behind(id)
		s.hub.Pace(id, 1)
	}
	w.WriteHeader(http.StatusNoContent)
}
```

`Hub.Behind` counts a report only for a stream that is open, and one per stream every 2 s at most, so a page that posts more often cannot inflate the count. The stream is named after the session and the tab, so a post reaches only its own session's streams ([The routes in an app](#the-routes-in-an-app)). The handler limits each session to one report a second after a burst of five (`edge.Limiter`), which keeps a client that posts in a loop away from the hub, and answers 204 either way. The edge's CSRF check treats `/pace` as an action, which needs Datastar's header. The listener sits on `<body>`: a `@post` from the stream's element would abort the stream's own request, with Datastar's default `requestCancellation`.

`lith/prom` exports the counts as `<namespace>_live_newest_frame_behind_total`, the reports, and `<namespace>_live_newest_frame_behind_streams_total`, the streams whose page reported at least once; the "lith apps" dashboard draws both in its Live row. They carry no labels and nothing about the viewer. A page that stays behind reports every 3 s, so three times the reports a second, `3 * sum(rate(app_live_newest_frame_behind_total[5m]))`, is about how many pages are behind at a time, and `sum(increase(app_live_newest_frame_behind_streams_total[1d]))` is how many tabs fell behind in a day, to set against the streams the app serves. `<namespace>_live_paces_total` and `<namespace>_live_releases_total` count the streams that were paced and those released again. Few reports mean few pages ever needed more than the plugin. A steady share of the streams means those pages spend much of their time paced, at about one frame per round trip: leaner markup helps them most ([Slow links](#slow-links)).

## Pacing a stream by the page

`newest-frame.js` drops frames in the page: the server still sends every frame, and the page throws away those it cannot apply. Pacing goes further: once the page reports that it is behind, the server sends the next frame only once the page says it has applied the last, so a slow page gets only the frames it applies. The template and this site do both by default, the hybrid in place:

1. Every stream has `Spec.Ack` and no `Window`, so it starts unpaced: a page that keeps up gets every frame and sends nothing back.
2. `<body>` posts `newest-frame-behind` to `/pace`, which calls `Hub.Behind` and `Hub.Pace(id, 1)`: from then on the stream sends one write at a time.
3. Each paced write ends with an element that posts `/ack` after the paint that shows it, with the round trip of the page's last `/ack` or `/pace`, and the route calls `Hub.RoundTrip` and `Hub.Ack`.
4. `Spec.Release` ends the pacing once no acknowledgement came late for 5 s (`live.DefaultRelease`), measured against the round trip the page reports and 100 ms of slack (`Spec.ReleaseSlack`, `live.DefaultReleaseSlack`). The page goes on watching, and posts `/pace` again if it falls behind again.
5. Both routes check the session and limit the rate ([The routes in an app](#the-routes-in-an-app)).

What it costs: a paced page gets one frame per round trip. On a slow phone on a far link, 15 times CPU and a 300 ms round trip, the hybrid kept the page 0.60 s behind at 1.45 frames a second, where the plugin alone kept it 0.47 s behind at 2.74. In return a slow page gets about a third of the bytes, 2.19 KB/s against 7.01 on a fast link, and a page that keeps up pays nothing: at full speed on a 300 ms link it was never paced, applied 10.07 frames a second and sent no acknowledgement ([Measured](#measured)).

To turn it off, leave `Ack` out of the stream's `Spec`. `Hub.Pace` then paces nothing, `/pace` still counts the reports, no page posts `/ack`, and the plugin alone drops what a page cannot apply. A negative `Spec.Release` keeps a paced stream paced, and `Spec.Window` paces a stream from the start. This site keeps the other approaches measurable through `web.Config.Pacing` (`off`, `every`, `reopen`), which only its tests set.

What each approach gets, measured with `TestLifeDrift` and `TestHybridLoad` (details under [Measured](#measured)):

```matrix
title: A page too slow for its stream: what each approach gets
source: bench/drift/RESULTS.md bench/hybrid/RESULTS.md bench/gaps/RESULTS.md bench/frame-cost/RESULTS.md
columns: Slow phone, fast link: lag median, KB/s sent | Fast phone, 300 ms round trip: frames a second | Slow for 60 s, then fast: does it recover | 1,000 viewers, a tenth slow: requests a second, CPU | What it needs in the page and on the server
note: The Game of Life (10 frames a second) in headless Chromium 154, behind Caddy over HTTP/2, three runs each, the median. Slow phone: CPU slowed 15 times, 150 s; no fix on 2026-10-16, the others on 2026-10-17. Round trip: no CPU throttling, 90 s. Recovery: 15 times for 60 s, then not at all. 1,000 viewers: TestHybridLoad in one process without browsers, acknowledgements over HTTP, CPU in percent of one core. Leaner frames: the board as one SVG with a path per colour (bench/frame-cost). Good, fair and poor compare the approaches within a column.
---
No fix | poor: 27.15 s behind, 7.19 KB/s | good: 10.07 | fair: yes, 7 s after the speed-up, from 20.88 s behind | good: no requests, 150%, the same server path as the plugin's | good: nothing
`newest-frame.js` | fair: 0.28 s behind, but 7.01 KB/s | good: 10.07 | good: yes, at once, then 10.03 frames a second | good: no requests, 150% | good: the plugin in the page
`newest-frame.js` with `__reopen` | fair: 0.62 s behind, 3.36 KB/s | good: 10.07 | poor: no, it stays on the slow stream at 4.02 frames a second | good: no requests, 155% | good: the plugin, `Save-Data` on the stream's request, `Spec.Slow` from `edge.SlowLink`
Ack pacing for every stream, W=1, after paint | good: 0.31 s behind, 1.93 KB/s | poor: 2.83 | fair: yes, at once, but with 10.07 acknowledgements a second from then on | poor: 9,205 requests, 439% | fair: nothing in the page beyond Datastar; `Spec.Window`, `Spec.Ack` and an `/ack` route
Hybrid, in place | good: 0.34 s behind, 2.19 KB/s | good: 10.07, never paced | good: yes, released at 66 s in all three runs, then 10.05 frames a second | good: 250 requests, 156% | fair: the plugin without `__reopen` and a listener on `<body>`; `Spec.Ack`, `Spec.Release`, `/ack` and `/pace` routes
Leaner frames | good: 0.00 s behind, 1,304 B a frame, about 13 KB/s | not measured | not measured | not measured | fair: different markup, only where the view allows it
```

A slow phone pays in input too, and a far link makes it worse for the paced approaches:

```matrix
title: A slow phone: input, and a far link on top
source: bench/gaps/RESULTS.md
columns: Key press at 15 times: to the handler, median; Event Timing p98 | Slow phone, 300 ms round trip: lag median, frames a second, KB/s | Paced, then fast, 300 ms round trip: frames a second after
note: The Game of Life in headless Chromium 154, behind Caddy over HTTP/2, CPU slowed 15 times, three runs each, 2026-10-17. Key press: Shift every 500 ms through DevTools, 150 s, all presses of three runs; Event Timing p98 counts presses without an entry as under 16 ms, and INP calls 200 to 500 ms "needs improvement". Far link: the delaying proxy, 150 s. Paced, then fast: 15 times for 60 s, then not at all.
---
No fix | poor: 2,970 ms; 6,368 ms | poor: 26.11 s behind, 7.13, 7.09 KB/s | not measured
`newest-frame.js` | fair: 142 ms; 480 ms | good: 0.47 s behind, 2.74, but 6.91 KB/s | not measured
`newest-frame.js` with `__reopen` | fair: 132 ms; 448 ms | fair: 0.80 s behind, 2.84, 3.30 KB/s | not measured
Ack pacing for every stream, W=1, after paint | fair: 118 ms; 448 ms | fair: 0.55 s behind, 1.45, 1.21 KB/s | poor: 2.83, paced for good, as at 300 ms without throttling
Hybrid, in place | fair: 118 ms; 464 ms | fair: 0.60 s behind, 1.45, 1.48 KB/s | poor: 2.82, never released
Hybrid, in place, release against the round trip the page reports | not measured | fair: 0.64 s behind, 1.43, 1.45 KB/s | good: 9.37, released 5 to 6 s after the speed-up
```

The sections below show each part as the template wires it.

### The window

`live.Spec.Window` paces a stream: at most `Window` writes of frames go out that the page has not acknowledged. Each write of frames ends with the event that `Spec.Ack(token, n)` makes, and the page acknowledges the write once it has applied it, which the app passes to `Hub.Ack(id, token, n)`. `n` counts the stream's paced writes from 1, and `token` tells this stream from an older one with the same id, so an acknowledgement meant for a stream that has since opened again frees nothing. `Stats.Acks` counts them.

While the window is full, the stream's frames wait in its one slot per view, and the hub does what it does for any stream with a frame waiting ([Ticks](/docs/concepts/ticks#the-newest-frame-wins)): a view that no other stream keeps up with is not rendered (`Stats.RendersDeferred`). When an acknowledgement frees the window and a waiting frame's view was deferred since, the stream drops that frame, and the next tick renders the view as it is then, so the page never gets a frame that waited on the server for it. A stream that hears nothing for `live.AckTimeout`, 10 s, sends its next write anyway, so a lost acknowledgement cannot stall it for good.

In the template the event is an element patched into the stream's element, whose `data-init` posts the acknowledgement after the paint:

```go source=template/internal/web/pace.go
func ackEvent(tab string) func(token string, n int64) []byte {
	return func(token string, n int64) []byte {
		num := strconv.FormatInt(n, 10)
		post := "@post('/ack?tab=" + tab + "&s=" + token + "&n=" + num + "&rtt=' + " + lastRoundTrip + ", {requestCancellation: 'disabled'})"
		el := `<i id="ack-` + num + `" data-init="` + html.EscapeString("requestAnimationFrame(() => setTimeout(() => "+post+"))") + `"></i>`
		return sse.PatchElements([]byte(el), sse.Patch{Selector: "#stream", Mode: sse.Inner})
	}
}

// lastRoundTrip is the round trip of the page's last /ack or /pace in ms,
// from its Resource Timing, or 0. It clears the entries, so that the
// buffer of 250 never fills with acknowledgements.
const lastRoundTrip = `((x) => (performance.clearResourceTimings(), x ? Math.round(x.responseStart - x.requestStart) : 0))(performance.getEntriesByType('resource').findLast(r => r.name.includes('/ack?') || r.name.includes('/pace?')))`
```

Why the acknowledgement is an element in the stream's element:

- Datastar runs `data-init` once the element is in the page, and the element comes after the write's frames, so the post means that the frames before it are applied. `requestAnimationFrame` and a `setTimeout` hold it until after the paint that shows them, which gave the lowest lag at 15 times (0.33 s at the median against 0.46 s once applied, `bench/drift`).
- Every streaming page has the stream's element, and no view renders into it, so no frame morphs the acknowledgement away before it runs. Mode inner replaces the one before, so one acknowledgement sits there at a time, and a new id each time has Datastar run `data-init` again.
- Datastar cancels a request in flight when another one goes out with the same method to the same URL, from any element of the page. The acknowledgement is a POST, so it leaves the stream's `@get` alone, and `requestCancellation: 'disabled'` keeps a second acknowledgement from cancelling the first when the window is 2.
- It needs nothing in the page beyond Datastar, and the server decides per write whether to ask.

### Pacing only a page that is behind

A stream with `Spec.Ack` and no `Window` starts unpaced and sends no acknowledgement events. `Hub.Pace(id, window)` sets the window of the open stream with that id: from its next write on, each write ends with its acknowledgement event and the window applies, and nothing written before waits for one. `Pace(id, 0)` stops the pacing. A stream paced already keeps its window and its count towards `Release`, so a second `Pace` changes nothing. `Stats.Paces` counts the streams that `Pace` started to pace.

The page says when: `newest-frame.js` dispatches `newest-frame-behind` when it dropped more than half of the frames of the last 3 s, at least ten, and goes on watching. The template's `<body>` posts the report with the tab's id to `/pace` ([Slow links](#slow-links) shows the attribute):

```go source=template/internal/views/expr.go
const behind = `@post('/pace?tab=' + $_tab, {requestCancellation: 'disabled'})`
```

The plugin dispatches the event on the stream's element, and it bubbles up to `<body>`. The listener must not sit on the stream's element: a `@post` from there would, with Datastar's default `requestCancellation`, abort the stream's own request. `/pace` counts the report with `Hub.Behind` and paces the stream. While the page stays behind, the plugin reports again every 3 s and the page posts again, which `Pace` ignores. The stream stays open through the switch: no frame is lost or sent twice, the stream keeps its compression window, the page its signals and the plugin its state.

### Switching back

`Spec.Release` ends the pacing of a page that keeps up again. At each acknowledgement the hub notes whether a frame was already waiting for it to go out. Once no acknowledgement has come late for `Release` since the pacing began, the stream sets its window to 0 and sends every frame again (`Stats.Releases`). The page goes on watching, and if it falls behind again, the plugin reports and the page posts `/pace` again.

Each release of the same stream doubles the time for the next, up to eight times `Release`: with `live.DefaultRelease`, the 5 s the measurements used, 5, 10, 20 and then 40 s, so a page that switches back and forth does so at most once per 40 s. The two rules also leave a wide gap between them. Pacing starts for a page that applies fewer than about half of the frames, and ends for one whose every acknowledgement comes before the next frame is ready, that is, one that applies all of them; a page in between stays as it is. In `TestLifeDrift` from 2 to 15 times CPU no page switched back and forth: up to 5 times it was never paced, and from 6 times on it was paced 2.1 to 2.5 s in and stayed paced, at 6.71 frames a second at 6 times and 4.79 at 8 times (three runs each).

An acknowledgement takes at least a round trip, and by then the hub has the next frame ready whenever the round trip is a tick of the view or longer (100 ms for the Game of Life). On such a link the rule above never releases: a page that fell behind once stays paced at about one frame per round trip, 2.82 a second at 300 ms, even after it is fast again. `Spec.ReleaseSlack` measures against the round trip instead: once the page has reported it with `Hub.RoundTrip(id, rtt)`, an acknowledgement is late only if it came more than `ReleaseSlack` after the round trip, counted from the write it acknowledges. A stream gets `live.DefaultReleaseSlack`, 100 ms, unless `ReleaseSlack` sets another, and a negative one keeps the rule above; either way it applies only once the page reports a round trip. The template's page takes the round trip from the Resource Timing of its last `/ack` or `/pace` (from sending the request to the first byte of the answer, which the browser's network stack notes however busy the page is) and appends it to its next acknowledgement, and `/ack` passes it on. With `ReleaseSlack` at one tick, a page that was paced at 15 times and then became fast was released 5 to 6 s later at 100 and at 300 ms round trip, three runs each, and applied 9.72 and 9.37 frames a second after it; a page that stayed slow was never released, in nine runs. Taking the fastest acknowledgement as the round trip instead does not work: a page paced at one frame per round trip has time between frames, so its acknowledgements come at a steady delay, and that rule released a page still slowed 15 times.

### The routes in an app

Any page can post to `/ack` and `/pace`, so both need what the stream handler has, the session, and a limit. The template names each stream after the session and the tab, and builds the same name in both routes from the request's session, so that a post reaches only its own session's streams:

```go source=template/internal/web/pace.go
// streamID names a tab's stream after its session too, so that /ack and
// /pace reach only the streams of the request's session.
func streamID(sid, tab string) string {
	return sid + "." + tab
}
```

The stream handler uses it as the stream's `Spec.ID`, and gives the stream its `Ack`:

```go source=template/internal/web/server.go
	spec := live.Spec{
		ID:        streamID(sid, tab),
		Client:    edge.ClientKey(r),
		Views:     []live.View{entriesView(), tabView(sid, tab)},
		Reconnect: "stream",
		Slow:      edge.SlowLink(r),
		Ack:       ackEvent(tab),
		Build:     page.Build,
		Page:      r.Header.Get("Page-Path"),
		Dirty:     page.Dirty,
		Refresh: func(ctx context.Context, rq live.RefreshRequest) ([][]byte, error) {
			return s.refresh(ctx, rq, page.Assets)
		},
	}
```

`/ack` refuses what no page sends with a 400: a tab id that is not one, a token that is not a number in base 36, a write number below 1, a round trip that is not a number. It takes a round trip of up to a minute:

```go source=template/internal/web/pace.go
func (s *Server) ack(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tab, token := q.Get("tab"), q.Get("s")
	n, err := strconv.ParseInt(q.Get("n"), 10, 64)
	rtt, rttErr := strconv.Atoi(q.Get("rtt"))
	if !validID(tab) || !validToken(token) || err != nil || n < 1 || q.Has("rtt") && rttErr != nil {
		w.WriteHeader(http.StatusBadRequest) // no page of ours sends it
		return
	}
	id, now := streamID(edge.SessionID(r.Context()), tab), time.Now()
	if s.ackAddrs.Allow(edge.ClientKey(r), 1, now) && s.acks.Allow(id, 1, now) {
		if rtt > 0 && rtt <= 60_000 {
			s.hub.RoundTrip(id, time.Duration(rtt)*time.Millisecond)
		}
		s.hub.Ack(id, token, n)
	}
	w.WriteHeader(http.StatusNoContent)
}
```

Both answer 204, also over their limit. Each limits the client's address first (`edge.ClientKey`): a client mints a new session with every request that sends no cookie, so a limit keyed by the session alone is one it can step around. Then `/pace` allows a session five reports at once and one a second after (`edge.NewLimiter(1, 5)`), `/ack` 40 at once and 20 a second for each stream (`edge.NewLimiter(20, 40)`, keyed by the stream's ID), twice the ten frames a second of the template's views, so a session with several paced tabs does not share one budget: an acknowledgement the limit drops holds a stream with a window of one until `live.AckTimeout`, 10 s. Both routes go in the edge's CSRF check as actions (`edge.CSRF.Datastar`). An app whose `Watch` calls name streams by the tab alone, as this site's demos do, can keep the session that opened each stream instead: this site notes it as the stream opens and checks it in both routes (`site/internal/web/ack.go`).

### At W=1, a paced stream is long polling

At a window of one, a paced stream is long polling: each acknowledgement asks for the next frame, so the page gets at most one frame per round trip plus the time to apply it. It differs from ordinary polling in three ways. Nothing goes out while the view does not change. The frame arrives over the open SSE stream, not as the response to the acknowledgement, which is a 204. And the server always sends the newest state, never a queue of old ones. A larger window works like TCP's sliding window: more frames in flight hide the round trip, and the server notices a slow page that many frames later. For a page that keeps up, plain push needs no requests and has no round-trip cap, which is why lith paces only a page that reports itself behind.

Each acknowledgement is a request. Over HTTP/2 with TLS it carried about 117 bytes up and 300 bytes down on the wire, and cost the site and Caddy together 1 to 3 ms of CPU, from single runs on the development machine (`bench/drift/RESULTS.md`, 2026-10-16).

### Measured

Measured on 2026-10-17 on the development machine with `TestLifeDrift` (`bench/hybrid/RESULTS.md`, and three runs of everything in `bench/gaps/RESULTS.md`): the site's Game of Life, 10 generations a second and a fresh soup every 15 s, in headless Chromium 154, behind Caddy 2.10.2 with TLS and HTTP/2, `navigator.connection` saying 4g. The paced runs acknowledge after the paint with a window of one and `Release` at 5 s. "In place" is the wiring above; lag is how long ago the board passed the generation the page applied last. Medians of three runs, the range in brackets.

- **A slow phone on a fast link.** CPU slowed 15 times, 150 s. Paced in place, the page posted `/pace` 2.2 s in and stayed 0.34 s behind at the median (0.30 to 0.37), 0.70 s at most, while the site wrote 2.19 KB/s. The plugin with `__reopen` stayed 0.62 s behind, 1.21 s at most, at 3.36 KB/s; without `__reopen`, 0.28 s and 1.03 s at 7.01 KB/s; ack pacing from the start, 0.31 s and 0.72 s at 1.93 KB/s. In every one of them the main thread stayed fully busy, half applying frames and half rendering them: pacing changes what the page receives, not what a frame costs it.
- **Input on that phone.** Shift pressed every 500 ms through DevTools: without a fix a press reached the page's handler after 2,970 ms at the median and 6,581 ms at most, and Event Timing's p98 was 6,368 ms. With any of the fixes it took 118 to 142 ms at the median and at most 443 to 529 ms; the p98 was 448 to 480 ms, which INP calls "needs improvement", since rendering each frame still fills the main thread.
- **A fast phone on a far link.** No CPU throttling, a proxy in front of Caddy that holds every chunk for half the round trip each way, 90 s. No fix, the plugin with and without `__reopen` and both hybrids applied 10.07 frames a second at 100 and at 300 ms, and the hybrids never paced. Ack pacing from the start capped the page at 6.61 and 2.83 frames a second, W=2 at 10.07 and 5.84.
- **A slow phone on a far link.** CPU slowed 15 times and 100 or 300 ms round trip, 150 s. A paced page gets one frame per round trip plus the time to apply and paint it: paced in place, 2.08 frames a second at 100 ms and 1.45 at 300 ms, 0.38 and 0.60 s behind at 1.89 and 1.48 KB/s. The plugin without `__reopen` stayed 0.35 and 0.47 s behind at 7.01 and 6.91 KB/s, the lowest lag; with `__reopen`, 0.70 and 0.80 s at 3.3 KB/s. Without a fix, 25.61 and 26.11 s.
- **A page slow for a while.** CPU slowed 15 times for 60 s, then not at all. Paced in place, the stream was released at 66 s in all three runs and the page applied 10.05 frames a second after it. With `__reopen` the page stayed on the slow stream at 4.02 frames a second for good, 0.30 s behind. Without a fix the page was 20.88 s behind at 60 s and current again 6 to 7 s after the speed-up: the backlog drains on its own. On a far link, `Release` alone kept the page paced at 6.62 frames a second at 100 ms and 2.82 at 300 ms; with `ReleaseSlack` it was released 5 to 6 s after the speed-up and applied 9.72 and 9.37.
- **A page somewhat slow.** At 6 and 8 times the page without a fix applies every frame but paints only 6.49 and 2.56 a second, 0.10 and 0.37 s behind. `__reopen` then switches to the slow stream and makes it worse at 6 times, 0.37 s behind at 4.02 frames a second; paced in place it stays 0.09 and 0.13 s behind and paints every frame it gets, 6.70 and 4.78 a second. Below 6 times neither the plugin nor the hybrid switched.
- **The default, measured again.** The site as it ships, with no switch of the harness set, on 2026-10-19 with Chromium 155 (`bench/default/RESULTS.md`), three runs each: at 15 times on a fast link paced 2.1 s in and 0.35 s behind at 2.17 KB/s; at full speed on a 300 ms link never paced, 10.07 frames a second and no acknowledgement; at 15 times on that link 0.57 s behind at 1.46 frames a second and 1.48 KB/s; slow for 60 s and then fast on that link, released at 66 s in all three runs and 9.40 frames a second after. Each lies within the ranges above.
- **Firefox 158**, which cannot slow its CPU, with each frame costing the page 150 ms more: paced in place, 0.29 s behind at the median against 0.49 s with `__reopen`, and 32.04 s without a fix.
- **Many viewers.** `TestHybridLoad` in `live`, without browsers: one view of about 48 KB of HTML at 10 frames a second, 1,000 zstd streams, a tenth of them acknowledging a write 400 ms after it went out and the rest after 20 ms, the acknowledgements as HTTP posts to a server in the same process. Sending every frame to every stream, as the server does for the plugin and for no fix alike, took 150% of a core; the slow tenth on the slow stream, as `__reopen` gets them, 155% and 6% fewer bytes; pacing every stream 9,205 acknowledgements a second and 439%; pacing only the slow tenth 250 acknowledgements a second and 156%. The test leaves out the network, Caddy, TLS and the browsers.

### What it leaves open

- `ReleaseSlack` releases a page on a link of a tick or longer only once the page reports its round trip. A page that reports none, or a stream with a negative `ReleaseSlack`, stays paced on such a link at about one frame per round trip until the page opens its stream again.
- On a far link a paced page gets one frame per round trip: at 15 times and 300 ms it stays 0.60 s behind, where the plugin alone, without pacing, stays 0.47 s behind at four times the bytes.
- For the first 2 s or so, until the plugin has seen enough frames to judge, a slow page gets the full stream.
- An acknowledgement counts writes, not views: a write that holds frames of several views is one acknowledgement.
- The page reads its round trip from Resource Timing and clears the buffer at each acknowledgement, so other code of the page that reads resource entries finds few.

## Streams without the hub

A handler that streams without the hub opens an `sse.Stream` itself: `sse.Open(w, r, opts)`, then `st.Write(events...)` for each frame and `st.Close()` before it returns. Events are formatted once with `sse.PatchElements`, `sse.PatchSignals` and `sse.Comment`, and the same bytes can go to any number of streams. A stream's methods are not safe for concurrent use: one goroutine writes. When the server stops, the handler ends with `st.CloseWith(sse.Reconnect(id))`, as the hub does, so the page opens the stream again on the next process.
