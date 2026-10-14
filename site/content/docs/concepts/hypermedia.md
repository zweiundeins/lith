---
title: Hypermedia
headline: Hypermedia: Datastar events over compressed SSE
summary: Frames go to the browser as Datastar events over server-sent events, each stream compressed with a window of its own, within a memory budget.
order: 4
---

The server sends HTML, and the browser morphs it into the page. A view's frame is a Datastar event, `datastar-patch-elements` with the HTML or `datastar-patch-signals` with JSON, formatted once by `sse.PatchElements` or `sse.PatchSignals`, and the same bytes go to every stream that watches the view.

## A compressor per stream

Each stream compresses what it sends with a window of its own: zstd when the browser accepts it, else brotli, else gzip. The window spans the whole stream, so a frame that repeats most of the frame before it refers back to it and costs little. Rendering whole views relies on this.

lith's load test sends a board of 139 KB to 200 streams. With zstd, a frame takes 2,205 bytes on the wire, and the 200 streams keep 1.6 cores busy; brotli sends 955 bytes for 3.3 cores. Most of that CPU is the encoders: rendering takes under 4% of it.

## A window within a budget

An encoder's window is memory, held for as long as its stream is open. zstd at its fastest level with a window of 2^19 bytes holds about 1.7 MiB a stream, and brotli at datastar-go's default window 9.9 MiB: 1,000 open tabs would need about 1.6 GiB with the one and 9.7 GiB with the other.

A hub shares a budget among its streams: half the process's memory limit, from `GOMEMLIMIT` or the cgroup, for their encoders and the frames the hub keeps for their views. The window must hold the largest frame, or a frame cannot refer back to the one before it, and its bytes grow several times over: so a stream's first write picks a window that holds it. A zstd stream gets the largest window its share allows, down to that one; a brotli stream, which keeps its window until it closes, gets that window at quality 4, then at quality 3 as the budget fills, then 2^16, and goes out uncompressed when not even that fits. Without a memory limit, the budget only counts. A page with large frames passes a budget with `Refuse`, and a stream that finds no room for a window that holds its frame gets a 503, which the page retries.

## Every write has a deadline

Each write to a stream gets its own deadline, 10 s by default, and a client that does not take a frame in time loses its stream alone. So the server runs without a `WriteTimeout`. No lock and no slot is held while a write goes out on the network.

## Through a deploy

A page opens its stream from an element with an id, with `retry: 'error'`:

```html
<div id="stream" hidden data-init="@get('/stream?tab=' + $_tab, {retry: 'error', retryMaxCount: Infinity})"></div>
```

When the hub stops, each stream's last event is `sse.Reconnect` for that element (`live.Spec.Reconnect`): it replaces the element with a copy, the copy's `data-init` opens the stream again, and `'error'` keeps it trying through the restart's 502s and 503s. A stream that the server ends for another reason stays closed. Datastar's default, `'auto'`, retries network errors only and would leave the page stale after a deploy.

## The rest of the edge

Static files go out under names that carry their content's hash, cached for a year, and text files are compressed once, at brotli -11 and gzip -9 (`edge.Static`). GET answers are compressed per request (`edge.Compress`), and streams compress themselves.
