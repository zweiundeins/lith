# Dropping the frames a slow page cannot apply

The question from the Datastar Discord: should the frames a slow page cannot keep up with be dropped in the page (`newest-frame.js`, with or without `__reopen`), or on the server, which sends the next frame only once the page says it has applied the last one? This file has the measurements for both, the traces behind them, and what they say.

## Setup

- Machine: tower-dev, Intel Core i5-13500 (20 threads), 62 GB, Linux 6.18.38 (Unraid). Other work ran on it; the load average before each run is in the tables and in every `.log`. The runs went one after another, never in parallel.
- Software: Go 1.27.2, headless Chromium 154.0.8037.57, Firefox 158.0, Caddy 2.10.2 (TLS, HTTP/2, `flush_interval -1`), Datastar 1.0.4 (Starbase's patched build), `newest-frame.js` at commit fd92fa12 of datastar-attribute-newest-frame.
- Date: 2026-10-16, between 19:36 and 22:33 UTC.
- Code: branch `newest-frame-vendor` at 7b45591. Task 1 ran on `drift-traces` at bfcd478 (harness) and 3e073dd (traces). Task 2 ran on `ack-pacing`: 0254b70 (the same harness), 6d75d83 (ack pacing), 9501463 (harness fixes, used for the Cache-Control runs from `cache-h2-*-1` on, `cachetrace-*` and `cdp*`). The commit is in the first line of every `.log`.
- Harness: `TestLifeDrift` in `site/internal/app`, the site's Game of Life (10 generations a second, a fresh soup every 15 s) behind Caddy over HTTP/2, `navigator.connection` saying 4g, Chromium's CPU slowed 15 times with `Emulation.setCPUThrottlingRate` unless a row says otherwise.
- How far behind: each board frame the page applies is noted in the page with its time and generation; the lag at a second is how long ago the board passed the generation the page had applied last. This is what the page applied, not what it painted: without the plugin the page paints about every 7 s (see the traces), so what a user sees is up to 7 s older still. The page's clock and the server's agree to within about 50 ms, so lags under 0.1 s are noise.
- Main thread busy: Chromium's `TaskDuration` per second (`Performance.getMetrics`). Site CPU is the test process (site, hub, harness and, in the RTT runs, the delaying proxy); Caddy CPU is Caddy's process. Both in percent of one core.
- Raw outputs are in this directory: `NAME.log` (test output, load before and after), `NAME.csv` (every poll), `NAME.lag.csv` (every second), traces as `.trace.json` or, over 20 MB, `.trace.json.gz`. `table.py` makes the tables from the logs, `tracesummary.py` reads a trace (`traces-summary.txt`), `crop.py` cuts a window out of one, and the `batch-*.sh` scripts ran everything.

Two things in the harness changed on the way, and they matter for reading older numbers. The page only answers `Runtime.evaluate` when its main thread is free, so the old per-second reading of the page was taken right after a 7 s task and its "applied/s" column counted per poll, not per second; lag now comes from the times the page itself notes. And a page whose stream goes through Chromium's HTTP cache (no `no-store`) did not run an evaluation for minutes, so in Chromium the frames now reach the harness as console messages, which DevTools sends as they happen.

## Results

### Slow CPU on a fast link (15x CPU, Caddy, HTTP/2, 150 s, 3 runs each)

Median of the three runs, range in brackets.

| Configuration | Lag median (s) | Lag max (s) | Lag at 150 s (s) | Frames applied/s | Frames painted/s (trace) | Main thread busy | Site CPU | Caddy CPU | Site wrote KB/s | Acks/s | Load |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| No plugin | 27.15 (25.90-27.77) | 50.05 (49.51-50.98) | 50.0 | 6.91 | 0.1 | 100% | 1.5% | 0.2% | 7.19 | 0 | 2.1 |
| Plugin, no reopen | 0.32 (0.27-0.33) | 0.94 (0.72-1.08) | 0.2 | 2.61 | 2.2 | 100% | 1.9% | 0.1% | 7.09 | 0 | 1.7 |
| Plugin, `__reopen` | 0.64 (0.63-0.66) | 1.16 (1.03-1.21) | 0.6 | 2.81 | 2.2 | 100% | 1.8% | 0.1% | 3.20 | 0 | 1.9 |
| Ack pacing, W=1 | 0.46 (0.44-0.48) | 1.16 (1.13-1.17) | 0.4 | 2.72 | 2.3 | 100% | 1.9% | 0.2% | 2.08 | 2.72 | 1.7 |
| Ack pacing, W=2 | 0.57 (0.52-0.62) | 1.21 (1.19-1.30) | 0.3 | 3.68 | not traced | 100% | 1.8% | 0.2% | 2.70 | 3.67 | 1.8 |
| Ack pacing, W=1, ack after paint | 0.33 (0.30-0.33) | 0.70 (0.64-0.73) | 0.4 | 2.52 | 2.0 frames (4.2 paints) | 99% | 1.8% | 0.2% | 1.93 | 2.53 | 1.9 |

`table-compare.md` has the same with every column's range. Frames painted per second come from the traces below (20 s each), not from these runs.

### Fast CPU on a link far away (no throttling, Caddy, HTTP/2, 90 s, 1 run each)

`LIFE_DRIFT_RTT` puts a TCP proxy in front of Caddy that holds every chunk for half the round trip time each way, without a bandwidth limit. The link's bytes are TLS payload through that proxy.

| Configuration | RTT | Lag median (s) | Lag max (s) | Frames applied/s | Acks/s | Link up KB/s | Link down KB/s | Site CPU | Caddy CPU |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| No plugin | 100 ms | 0.05 | 0.06 | 10.07 | 0 | 0.15 | 9.61 | 3.6% | 0.2% |
| Plugin, `__reopen` | 100 ms | 0.06 | 0.07 | 10.07 | 0 | 0.08 | 8.52 | 3.5% | 0.2% |
| Ack pacing, W=1 | 100 ms | 0.11 | 0.22 | 7.10 | 7.10 | 0.95 | 9.42 | 6.9% | 1.4% |
| Ack pacing, W=2 | 100 ms | 0.00 | 0.00 | 10.08 | 10.07 | 1.32 | 12.66 | 4.8% | 1.9% |
| Ack pacing, W=1, after paint | 100 ms | 0.13 | 0.24 | 6.61 | 6.61 | 0.95 | 9.09 | 7.1% | 1.6% |
| No plugin | 300 ms | 0.10 | 0.11 | 10.07 | 0 | 0.08 | 9.75 | 3.9% | 0.2% |
| Plugin, `__reopen` | 300 ms | 0.15 | 0.16 | 10.06 | 0 | 0.08 | 8.03 | 3.5% | 0.2% |
| Ack pacing, W=1 | 300 ms | 0.28 | 0.48 | 2.91 | 2.91 | 0.44 | 5.78 | 5.7% | 0.8% |
| Ack pacing, W=2 | 300 ms | 0.18 | 0.38 | 5.83 | 5.83 | 0.80 | 8.61 | 6.6% | 1.4% |
| Ack pacing, W=1, after paint | 300 ms | 0.32 | 0.52 | 2.83 | 2.83 | 0.43 | 5.65 | 5.9% | 0.8% |

The lag here is mostly the one-way delay (50 and 150 ms) plus how old the frame was when it left; with W=1 a frame can wait up to one round trip on the server before it goes out. W=1 caps the page at about one frame per round trip plus the time to apply and acknowledge it (7.1/s at 100 ms, 2.9/s at 300 ms); W=2 at two. The plugin costs nothing here: the page keeps up, applies all ten frames a second and never drops one.

Each acknowledgement is a POST of about 845 bytes as HTTP/1.1 would carry it (path, cookies, Datastar's headers, an empty signals body). Over HTTP/2 with TLS the link carried about 117 bytes up and 300 bytes down per acknowledgement more than without (W=2 at 100 ms against no plugin: +1.17 KB/s up and +3.05 KB/s down at 10 acks a second; the down side is the 204 and the acknowledgement event in the stream). The CPU per acknowledgement is harder to pin down from single runs with one viewer, the harness and the proxy in the same process: the differences above come to 1 to 3 ms of CPU per acknowledgement for the site and Caddy together on this machine. At ten frames a second that is 1 to 3 percent of a core per viewer that keeps up, which no viewer pays with the plugin.

DevTools' own network emulation (`LIFE_DRIFT_CDP_LATENCY`, `cdp100ms-ack1`, `cdp300ms-ack1`) cannot stand in for such a link: with 100 and 300 ms of emulated latency, ack pacing with W=1 still got 10.1 acknowledgements and frames a second. The emulation delays responses, not the requests that reach the server or the data of a response already open, so it never slowed the loop.

### Many viewers of one shared view

Not measured with many browsers; this follows from how the hub works and from the numbers above. The Game of Life's board is one view: the hub renders it once per tick for everyone, and each stream keeps only its newest frame of it. With the plugin, every stream gets every frame, and the server's cost per viewer is the write and compression of about 7 KB/s; a slow page drops frames in the browser, so the server never learns about it, and pays for frames that are thrown away (or, with `__reopen`, for a quarter of them: 3.2 KB/s). With ack pacing, a slow viewer gets only what it can apply (2.1 KB/s here), and the view still renders at every tick as long as one viewer keeps up; when none keeps up, renders defer as for a slow stream. In exchange, every viewer, fast or slow, sends one request per frame it applies: ten a second per fast viewer on this demo, which for 1,000 viewers is 10,000 requests a second through Caddy and the site, against none with the plugin.

## The traces (Task 1)

Recorded with `LIFE_DRIFT_TRACE` (CDP `Tracing.start` with `transferMode: ReturnAsStream`, DevTools' categories plus the CPU profiler, read with `IO.read`), 20 s starting 30 s into the run, behind Caddy over HTTP/2. Chromium answers `Tracing.start` only once the page's renderer has joined, which in (a) took 10 s (the trace starts 40 s in). Tracing costs the page something: at full speed the main thread was 50% busy in the traces against 44 to 46% in runs without one. They open in Chrome DevTools' Performance panel (the screenshots below are that panel, loaded with these files) and in ui.perfetto.dev (not checked by hand here; the format is the same JSON). Every trace has User Timing marks the harness adds: `lith:read <bytes>` for each read of the stream's body, `lith:recv` for each patch event that arrives, and `lith:apply board` measures for the time Datastar takes to apply each board frame.

Files (Task 1 in `/develop/_lith/lith-drift-traces/bench/drift/`, Task 2 here):

- (a) no plugin: `trace-a-off.trace.json` (12 MB)
- (b) plugin without reopen: `trace-b-noreopen.trace.json.gz`
- (c) plugin with `__reopen`: `trace-c-reopen.trace.json.gz`
- (d) full speed with the plugin: `trace-d-fullspeed.trace.json.gz` (177 MB unzipped), and without it: `trace-d0-fullspeed-off.trace.json.gz`
- (e) ack pacing W=1 at 15x: `trace-e-ack1.trace.json.gz`, after paint: `trace-e-ack1paint.trace.json.gz`, at full speed: `trace-e-ack1-fullspeed.trace.json.gz`

What `tracesummary.py` reads from them (`traces-summary.txt` in both directories):

| Trace | Main thread busy | Longest tasks (ms) | Board frames applied per task | Bytes per stream read | Paints in 20 s, longest gap | Applying frames | Style, layout, paint |
| --- | --- | --- | --- | --- | --- | --- | --- |
| (a) no plugin, 15x | 100% | 7188, 6702, 4636 | 45, 45, 31 | 2,097,152 every time (3 reads) | 2, 7.6 s | 91% | 0.6 s |
| (b) plugin, no reopen, 15x | 97% | 515, 390, 387 (no frame in them) | 1 | 84 KB to 465 KB, median 186 KB (45 reads) | 44, 0.75 s | 33% | 8.6 s |
| (c) plugin, `__reopen`, 15x | 97% | 451, 437, 425 (no frame in them) | 1 | 31 KB to 186 KB, median 93 KB (44 reads) | 44, 0.70 s | 34% | 9.7 s |
| (e) ack W=1, 15x | 100% | 502, 445, 429 (no frame in them) | 1 | about 46.7 KB, one frame (47 reads) | 49, 0.74 s | 35% | 9.0 s |
| (e) ack W=1 after paint, 15x | 98% | 442, 419, 416 | 1 | about 46.8 KB (41 reads) | 83, 0.52 s | 30% | 10.0 s |
| (d) plugin, full speed | 50% | 52 | 1 | 46.5 KB median | 1067, 67 ms | 17%, 15 ms a frame | 4.5 s |
| (d0) no plugin, full speed | 50% | none over 50 | 1 | 46.5 KB median | 1053, 60 ms | 19%, 18 ms a frame | 4.4 s |
| (e) ack W=1, full speed | 51% | none over 50 | 1 | 46.7 KB median | 1167, 48 ms | 16% | 5.0 s |

"Applying frames" is the share of the 20 s inside `lith:apply` measures, which is Datastar's morph and whatever runs synchronously in it.

The claim in the docs, checked: once behind, every read Chromium hands the page is exactly 2 MiB (2,097,152 bytes), which holds 45 board frames (a frame is about 46.6 KB once decompressed, 700 bytes on the wire), and Datastar applies all 45 in one task of 6.7 to 7.2 s, about 145 ms a frame, without a paint between: the two paints in (a) are 7.6 s apart. That part of the docs is right. Two details to add: the first reads after the page falls behind are smaller (370 KB to 1.1 MB in the traces from page load) before they reach 2 MiB, and without `no-store` two or three reads can land in one task (below).

The plugin does not make the long tasks go away. In (b) and (c) the main thread is still 97% busy, and the tasks of 300 to 515 ms apply no frame at all. Taken apart with `breakdown.py` (output in `breakdown.txt`, from trace b; c and the ack-paced e are the same within a few points):

- The tasks that apply a frame take 51% of the time; nearly all of it is one microtask checkpoint in which the plugin's flush (`w` in `newest-frame.js`) calls Datastar's apply. The plugin's own work besides is its listener for each arriving patch event (`A`), 1.35 s of the 20 s in b (all ten frames a second arrive) and 0.63 s in c (four a second), so 3 to 7% of the main thread at 15x; that function may run inside the tasks counted here.
- The long tasks without a frame take 45%: they are the rendering updates after each morph. Of their time, paint is 40%, layout 24%, a `requestAnimationFrame` callback of the population sparkline (`sb-sparkline`, about 35 ms per frame at 15x) 25%, layerize 5%, pre-paint 2%. No GC to speak of (0.4 s in 20 s).

So the plugin's rule ("after a batch it waits half as long as that batch took to apply, so frames take at most two thirds of the main thread") holds for applying frames, which take about half of the main thread, but the pause it leaves is filled by the style, layout and paint that the frame it just applied causes, and on a page this slow nothing is left over. Suggested wording for the docs: "After a batch it waits half as long as applying the batch took, so applying frames takes at most two thirds of the main thread. The rendering that each applied frame causes runs in that pause and is not counted: on a phone 15 times slower than a desktop, the Game of Life's main thread stayed 97% busy, half applying frames and half laying out and painting them, but it painted every frame it applied, about two a second, and stayed within a second of the board." If the plugin should leave real headroom, it would have to time a batch to the end of the paint that follows it (a `requestAnimationFrame` plus a task), not to the end of the morph.

### Screenshots (Chrome DevTools' Performance panel)

Made with `TestFlameShot` (`site/internal/app/flame_test.go`): Chromium's own DevTools front end, served by a second headless Chromium with a debugging port on 127.0.0.1, loads a window of the trace cut with `crop.py` (the CPU profile left out, so the flame chart shows the trace's own events). Shots 1, 3, 4 and 5 cover the same 8 s, 6.5 to 14.5 s into each trace.

- `flame-1-noplugin-8s.png`: no plugin, 15x. One 7.2 s task applies 45 frames (the `Event dispatch` blocks under `Run microtasks`, one `lith:apply board` each in Timings), with paint (green) only before and after it.
- `flame-2-noplugin-20s.png`: the same trace, all 20 s: three such tasks, two paints.
- `flame-3-reopen-8s.png`: the plugin with `__reopen`, 15x, same 8 s: one frame per task, each followed by a rendering task (purple and green) and a paint, about two a second.
- `flame-4-ack1-8s.png`: ack pacing with W=1, 15x, same 8 s: the same rhythm as shot 3; the Network track shows one `ack` request per frame.
- `flame-5-fullspeed-noplugin-8s.png`: no throttling, no plugin, same 8 s: short tasks, ten frames a second, a paint every 17 ms at the median.

## Cache-Control on the stream

lith sends `Cache-Control: no-store` on every stream (`sse/stream.go`). The harness can replace it (`LIFE_DRIFT_CACHE`) with `no-cache` or leave the header out. Without `no-store`, Chromium sends the response through its HTTP cache (the traces show a `CacheThread_BlockFile` thread then). Runs without the plugin, 120 s, 15x CPU, two runs each in Chromium; Firefox with each board frame costing 150 ms more (`LIFE_DRIFT_COST`, since Firefox has no CPU throttling), one run each, and Chromium the same way for comparison:

| Browser, protocol | Cache-Control | Lag at 120 s (s) | Lag median (s) | Frames applied/s |
| --- | --- | --- | --- | --- |
| Chromium, HTTP/2 (Caddy) | no-store | 43.8 (43.6-43.9) | 23.50 | 6.65 |
| | no-cache | 42.8 (42.3-43.3) | 24.31 | 7.05 |
| | none | 43.1 (42.0-44.3) | 24.28 | 6.94 |
| Chromium, HTTP/1.1 (no Caddy) | no-store | 42.8 (42.7-42.8) | 22.84 | 6.71 |
| | no-cache | 43.4 (41.7-45.0) | 24.88 | 6.98 |
| | none | 42.6 (42.1-43.1) | 23.72 | 7.00 |
| Firefox, HTTP/2, 150 ms a frame | no-store | 51.5 | 25.61 | 5.76 |
| | no-cache | 50.6 | 25.87 | 5.87 |
| | none | 50.3 | 25.74 | 5.88 |
| Firefox, HTTP/1.1, 150 ms a frame | no-store | 50.4 | 25.58 | 5.85 |
| | no-cache | 50.3 | 25.74 | 5.88 |
| | none | 50.5 | 25.75 | 5.87 |
| Chromium, HTTP/2, 150 ms a frame | no-store | 45.7 | 23.90 | 6.27 |

The header changes nothing about how far behind the page falls, in either browser or protocol. Over HTTP/1.1 the socket queues the harness reads were alike too: with each of the three, the site's send queue held 77 to 114 KB and the browser's receive queue 110 to 157 KB at 120 s (`cache-h1-*-1.csv`, columns `site_sendq` and `browser_recvq`), that is, Chromium stopped reading the socket at about the same point.

The traces show one difference, and it is for the worse (`cachetrace-*.trace.json.gz`, recorded from page load for 60 s because a trace started mid-run never got the renderer's events with `no-cache` or without the header, its main thread busy throughout): a read is still at most 2 MiB (the first ones after the page falls behind are smaller, 370 KB to 1.1 MB), but through the cache two or three reads can land in one task. The longest tasks were 14.1 to 14.6 s with 90 or 91 frames and 4 MiB (HTTP/2, `no-cache` and none) and 9.2 s with 56 to 58 frames (HTTP/1.1), against at most 7.8 s and 46 frames with `no-store`. So `no-store` is the right header to keep; the others give the page longer freezes and no relief.

## Ack pacing: what was built

In `live` (commit 6d75d83): `Spec.Window` and `Spec.Ack`. A stream with `Window: W` sends at most W writes of frames that the page has not acknowledged. Each write ends with the event `Spec.Ack(token, n)` makes; the page acknowledges it with `Hub.Ack(id, token, n)`, where n counts the stream's writes and the token is the stream's sequence number, so an acknowledgement for an older stream with the same tab id frees nothing. While the window is full, the stream's frames wait in their one slot per view, and the hub's existing deferral applies: a view that no other stream keeps up with is not rendered (`RendersDeferred`), exactly as for `Spec.Slow`. When an acknowledgement frees the window and a waiting frame's view was deferred since, the stream drops that frame and the next tick renders the view as it is then, so the page never gets a frame that sat on the server waiting for it. A stream that hears nothing for 10 s sends anyway, so a lost acknowledgement cannot stall it for good. Tests: `live/ack_test.go`.

In the site (`site/internal/web/ack.go`, off unless `Config.AckWindow` is set, which only the harness does): the acknowledgement event patches `<i id="ack-N" data-init="@post('/ack?tab=…&s=…&n=N', {requestCancellation: 'disabled'})">` into the stream's element (`#stream`, mode inner). Why this mechanism: it needs nothing on the page beyond Datastar, the server decides per write whether to ask, and Datastar runs `data-init` only after the patch before it has been morphed in, so the acknowledgement means "applied". A new id each time makes Datastar run `data-init` again; `requestCancellation: 'disabled'` keeps a second acknowledgement from cancelling the first with W=2. A signal patch plus `data-effect` would work as well; a plain `fetch` in a script would not fit the site's CSP. With `Config.AckAfterPaint` the expression is wrapped in `requestAnimationFrame(() => setTimeout(...))`, so it fires after the paint that shows the frame.

After the morph or after the paint matters, a little: acknowledging after the paint gave the lowest lag of all at 15x (0.33 s median, 0.70 s max, against 0.46 and 1.16 s), because the next frame then arrives when the page is ready for it instead of during the rendering of the last one. It applied a few fewer frames (2.5 against 2.7 a second) and painted twice per frame (83 paints for 39 frames in its trace). On a far link it is slightly slower, since the paint adds to each round trip.

What this prototype leaves out: the `/ack` endpoint checks only the tab id and the token, no session, and has no rate limit; acknowledgements count writes, not views; a page in a background tab that acknowledges after paint stops getting frames until it shows again, which is arguably right; and the site's other streams would get it too if it were turned on.

## Conclusion

- **Slow CPU on a fast link.** Both layers fix it, by about the same amount, and both leave the main thread 97 to 100% busy, half morphing and half rendering. Without either, the page falls 50 s behind in 150 s and paints once every 7 s. The plugin alone (no reopen) gave the lowest lag of the page-side options, 0.32 s at the median; ack pacing after paint matched it (0.33 s) with a lower maximum (0.70 s against 0.94 s) and sent the fewest bytes (1.9 KB/s against 7.1). `__reopen` costs some lag (0.64 s, its frames are up to 250 ms old and four a second) for less than half the bytes. The server-side approach is not worse here; it is a little better on bytes and on the worst case, and it needs a request per frame.
- **Fast CPU on a slow or far link.** The plugin is free: it never drops a frame when the page keeps up, and costs no request. Ack pacing with W=1 caps the frame rate at about one per round trip (7.1 a second at 100 ms, 2.9 at 300 ms), and W=2 at two (10 and 5.8); a window large enough for the round trip removes the cap but also delays the server noticing a slow page by as many frames. It adds one request per applied frame, about 117 bytes up and 300 down on the wire here, and 1 to 3 ms of server CPU each.
- **Many viewers of one shared view.** The render is shared either way. The plugin keeps the server's cost per viewer flat and blind: slow viewers get every frame and throw most away. Ack pacing sends a slow viewer only what it uses and lets renders defer when no one keeps up, but every fast viewer then costs ten requests a second on this demo, a cost the plugin does not have and that grows with the audience, not with how many viewers are slow.
- Taken together: the frame-level backpressure people asked for works and is a reasonable design when viewers are few and often slow, or when bytes matter more than requests. For a public demo with many fast viewers on varied links, dropping in the page costs nothing where nothing is wrong, and `__reopen` then moves a slow page to a cheaper stream. A hybrid that only paces a stream after the page reports being behind (the event `newest-frame-behind` already exists) would get the bytes of the first without the requests for everyone; not built or measured here.
- On the Cache-Control question: no effect on the lag or the socket queues; `no-store` avoids longer freezes that the other two cause in Chromium. Keep it.
