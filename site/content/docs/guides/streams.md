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
| brotli | quality 4, window 2^18 (256 KiB) | about 2.1 MiB |
| gzip | fastest level | about 0.8 MiB |

Chrome, Firefox and Edge accept zstd; Safari decodes it from version 26.3, and older Safari gets brotli. On lith's shared board, zstd sends 2.3 times brotli's bytes for half its CPU ([Performance](/docs/performance/encoders)). Datastar's Go SDK, given `WithBrotli` without `WithBrotliLGWin`, keeps brotli's 4 MiB window: 9.9 MiB of heap per stream, against 1.0 to 1.7 MiB for lith's zstd.

`sse.Options` sets the encoders, through `live.Options.SSE` for a hub's streams:

| `sse.Options` | Default | What it does |
| --- | --- | --- |
| `ZstdLevel` | `zstd.SpeedFastest` | `SpeedDefault` halves the bytes for no more CPU, at 1.1 MiB more per stream |
| `ZstdWindow` | 19 | zstd's window, as a base 2 logarithm |
| `ZstdLowMemory` | false | history at the window: 0.65 MiB less per stream, more CPU per frame |
| `BrotliQuality`, `BrotliWindow` | 4, 18 | brotli's quality and window |
| `NoCompression` | false | every stream as it is ([below](#small-frequent-events)) |
| `WriteTimeout` | 10 s | the deadline of each write |
| `Compress` | half the CPUs, in a hub | a semaphore that bounds how many streams compress at once |
| `Budget` | half the memory limit, in a hub | the memory the encoders share ([below](#the-memory-budget)) |

## Every write has a deadline

Each write to a stream gets its own deadline, `WriteTimeout`: a client that does not take a frame in time loses its stream, and nobody else waits for it. So the HTTP server runs without a `WriteTimeout` of its own, which would cut every stream after that long. A stream holds a slot of the `Compress` semaphore only while it compresses, never while its bytes go out on the network, so a few stalled clients cannot hold up the others.

## The window holds the largest frame

A frame can refer back only as far as the window reaches. When the window is smaller than the frame, the frame cannot find the previous one, and its bytes on the wire grow several times over. lith's benchmark board, a 142 KB frame, measured at zstd's fastest level:

| Window | Bytes per frame |
| --- | --- |
| 2^17 (128 KiB) | 10,831 |
| 2^18 (256 KiB) | 2,703 |
| 2^19 (512 KiB) | 1,471 |

So measure the largest frame of each page, the full board, the longest list, and pick the smallest window that holds it with room to spare. The board app's largest frame, three lanes of 50 cards with long titles, is 51 KB: 2^17 holds it. libretto's frames, 2 to 37 KB, cost 9% more bytes at 2^17 than at 2^19 and 19% more at 2^16. The [budget's table](/docs/performance/budget#settings) has every window for two workloads.

`Stream.Sent()` returns the bytes of events a stream was given and the bytes it sent, compressed. A test that renders the largest state of a page and checks the frame's length catches a page that outgrows its window.

## The memory budget

A hub shares a memory budget among its streams' encoders, so that the server holds what its memory allows, however many tabs are open. The default needs no setting:

- The limit is half the process's memory limit: `GOMEMLIMIT`, else the cgroup's `memory.max` or `memory.high`, whichever is lower. Without a memory limit the budget only counts, and every stream gets the largest window.
- A stream that opens gets the largest window that fits both its fair share (the limit divided by the open streams) and what is left: from 2^19 down to 2^18.
- When a stream finds no room, every zstd stream above the fair share of that moment moves to a smaller window at its next frame. That frame starts without history, once.
- A brotli body is one stream with one window, so a brotli stream keeps its tier until it closes. Instead it steps down as it opens: below 2^18 it takes quality 3 at 2^17 and 2^16 (912 KiB), which sends the board in 10 KB a frame instead of 142 KB.
- A stream that cannot have even its smallest tier (2^18 for zstd, 2^16 for brotli) goes out uncompressed: a zstd stream until room frees up at one of its later frames, a brotli or gzip stream for as long as it is open.

Because brotli streams cannot move down, a tier taken early would spend the room of the streams to come. So while up to 20 streams are open, a brotli stream gets the largest tier its fair share allows; past that, at most its largest tier times twice the share of the limit that is left: the largest tier while half the limit is left, then smaller ones as it fills. A budget of 150 MiB compresses 93 brotli streams at 2^19 and below, where taking 2^19 while it fit compressed 57 ([the simulation](/docs/performance/budget#what-lith-took-from-it)).

For pages with large, frequent frames, an uncompressed stream sends every frame whole: the board's 142 KB twenty times a second. Such an app sets the budget's floor to its largest frame and refuses the streams that do not fit:

```go source=site/internal/docs/example/web/hub.go
SSE: sse.Options{Budget: sse.NewBudget(sse.BudgetOptions{
	MinWindow: 17,
	Refuse:    true,
})},
```

A refused stream gets a 503, and a page that opens its stream with `retry: 'error'` asks again, with Datastar's backoff, until a stream closes and makes room.

| `sse.BudgetOptions` | Default | What it does |
| --- | --- | --- |
| `Limit` | half the memory limit | bytes the encoders may hold together; -1 for no limit |
| `MinWindow` | 18 | the smallest window a stream compresses with |
| `MaxWindow` | 19 | the largest window a stream gets |
| `Refuse` | false | 503 for a stream that cannot have its smallest tier, instead of no compression |

A stream's own `ZstdWindow` and `BrotliWindow` cap what the budget gives it, and `ZstdLowMemory` keeps it to the low-memory tiers. `hub.Budget().Stats()` says what the encoders hold now: the limit, the bytes reserved, the streams by tier (`zstd 2^19`, `zstd 2^18 low memory`, `br q4 2^18`, `br q3 2^16`, `identity`) and the streams refused. `SetLimit` moves the limit while the hub runs, and the zstd streams above their new share move down at their next frame. Streams outside a hub share a budget by passing the same `*sse.Budget` in their `sse.Options`.

[The budget's measurements](/docs/performance/budget) show the policies side by side, up to 2,000 streams.

## Hibernation

Most tabs sit still: a page left open beside the one someone works in, a dashboard nobody looks at. (A tab in the background has no stream at all: Datastar closes a `@get` while its tab is hidden and opens it again when the tab shows.) With `live.Options.Hibernate` set, a zstd stream that sent no frame for that long ends its zstd frame and gives its encoder back to a pool, and its window back to the budget. Its next frame takes an encoder from the pool and starts without history. Heartbeats go out without waking it.

The cost is that first frame after a pause: in lith's simulation of 200 tabs over an hour, hibernating after 30 s cut the heap per tab from 0.64 to 0.11 MiB and added about 45 KB per tab and hour on the wire ([Performance](/docs/performance/hibernation)). Brotli and gzip streams cannot start over without their history, so hibernation applies to zstd only. It is off unless an app sets it; this site sets a minute.

## Small, frequent events

An encoder's heap, 0.4 MiB and more per stream, buys little when the events are small: a cursor's position, a counter. `sse.Options.NoCompression` sends every stream as it is, whatever the browser accepts. In a hub it applies to every stream, so an app with one such page and others with large frames gives that page a hub of its own.

## Streams without the hub

A handler that streams without the hub opens an `sse.Stream` itself: `sse.Open(w, r, opts)`, then `st.Write(events...)` for each frame and `st.Close()` before it returns. Events are formatted once with `sse.PatchElements`, `sse.PatchSignals` and `sse.Comment`, and the same bytes can go to any number of streams. A stream's methods are not safe for concurrent use: one goroutine writes. When the server stops, the handler ends with `st.CloseWith(sse.Reconnect(id))`, as the hub does, so the page opens the stream again on the next process.
