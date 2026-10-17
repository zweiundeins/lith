# Pacing a stream only once its page is behind

`bench/drift/RESULTS.md` measured two ways to keep a page that is too slow for its stream current: `newest-frame.js` drops stale frames in the page, and ack pacing (`Spec.Window`) has the server send the next frame only once the page has acknowledged the last. The plugin costs nothing while the page keeps up; ack pacing sends a slow page the fewest bytes, but every viewer sends a request per frame, and with a window of one a fast page on a far link gets one frame per round trip. This file has the hybrid of the two: streams start unpaced, with the plugin on the page, and a stream is paced only once its page reports that it is behind.

## Setup

- Machine: tower-dev, Intel Core i5-13500 (20 threads), 62 GB, Linux 6.18.38 (Unraid). Another agent measured frame costs in `/develop/_lith/lith-frame-cost` at the same time; every run that started a browser, and the load test, took `/tmp/lith-bench.lock` first, so no two measurements overlapped. Builds and other agents' work outside the lock did run beside them. The load average before each run is in its `.log` and in the tables.
- Software: Go 1.27.2, headless Chromium 154.0.8037.57, Firefox 158.0, Caddy 2.10.2 (TLS, HTTP/2, `flush_interval -1`), Datastar 1.0.4, `newest-frame.js` at commit fd92fa12 of datastar-attribute-newest-frame, all as in `bench/drift`.
- Date: 2026-10-17, browser runs between 08:30 and 11:08 UTC, the load test after.
- Code: branch `hybrid` (worktree `/develop/_lith/lith-hybrid`), off `ack-pacing` at 0a915ee. `f8b2da7` adds `Hub.Pace` and `Spec.Release` to `live`, `d188bf0` the site's switches and the harness changes. Every browser run ran at `d188bf0` except `edge5x-hybrid-inplace` (`2b045cd`, which changed only a test file in `live`). The commit is in the first line of every `.log`.
- Harness: `TestLifeDrift` as in `bench/drift`: the site's Game of Life (10 generations a second, a fresh soup every 15 s) behind Caddy over HTTP/2, `navigator.connection` saying 4g, Chromium's CPU slowed with `Emulation.setCPUThrottlingRate`. Lag is how long ago the board passed the generation the page had applied last, from the times the page notes; lags under 0.1 s are noise. Site CPU is the test process (site, hub, harness and, in the RTT runs, the delaying proxy), Caddy CPU is Caddy's process, both in percent of one core.
- New in the harness: `LIFE_DRIFT_HYBRID=reopen|inplace` turns the hybrid on (window `LIFE_DRIFT_ACK`, default 1), `LIFE_DRIFT_RELEASE` sets `Spec.Release`, and `LIFE_DRIFT_THEN=1@60s,15@120s` changes the CPU slowdown mid-run. "Painted/s" counts the `requestAnimationFrame` callbacks that follow an applied frame (one per rendering update, right before its paint), so it comes from every run now, not only from traces. The log notes when the page posted `/pace`, when it opened a stream with `Save-Data: on`, and each second in which the hub paced or released a stream.
- All hybrid runs and the ack reference acknowledge after the paint (`LIFE_DRIFT_ACK_AFTER=paint`), which had the lowest lag in `bench/drift`, with a window of one and `Spec.Release` at 5 s.
- Raw outputs are in this directory: `NAME.log`, `NAME.csv` (every second, now with cumulative `acks`, `paces`, `releases`), `NAME.lag.csv` (now with `painted`), traces as `.trace.json.gz`. `table.py` makes the tables (`runs` for whole runs, `phases` for the phases of a run), `switch.py` reads a trace second by second around the switch, and the `batch-*.sh` scripts ran everything.

## Design

### In `live`

`Spec.Window` and `Spec.Ack` work as on `ack-pacing`. Three additions:

- A stream with `Spec.Ack` and no `Window` starts unpaced and sends no acknowledgement events. `Hub.Pace(id, window)` sets the window of the open stream with that ID: from its next write on, each write ends with its acknowledgement event and the window applies. Nothing written before waits for an acknowledgement. `Pace(id, 0)` stops pacing. A stream paced already keeps its window and its count towards `Release`. `Stats.Paces` counts the streams `Pace` started to pace.
- `Spec.Release` ends the pacing of a page that keeps up. At each acknowledgement, the hub notes whether a frame was already waiting for it to go out (the stream's slot for a view holds one). If none was, the page acknowledged before the next frame was ready. Once no acknowledgement has come late for `Release` since pacing began, the stream sets its window to 0 and sends every frame again (`Stats.Releases`). This costs a loop over the stream's views under a lock it already holds, per acknowledgement.
- Each release of the same stream doubles the time for the next, up to eight times `Release` (5, 10, 20, 40 s here), so a page that switches back and forth is slowed down to a switch per 40 s at most.

Tests in `live/ack_test.go`: `TestPaceAStreamLater`, `TestReleaseWhenThePageKeepsUp`, `TestNoReleaseWhileThePageIsLate`, `TestReleaseBacksOff`, beside the three window tests from `ack-pacing`. They pass with `-race`.

The rule has a wide gap between switching on and switching off. The plugin reports a page as behind when it skipped more than half of the frames of the last 3 s, at least ten: a page that applies fewer than about 5 of the 10 frames a second. Release needs every acknowledgement for 5 s to come within one tick of 100 ms: a page that applies all 10. A page between the two stays in whichever state it is in. On a far link the release cannot happen at all: an acknowledgement takes at least a round trip, so with a round trip over 100 ms a frame is always waiting when it arrives. Such a page stays paced at about one frame per round trip (2.8 a second at 300 ms) until it reopens its stream. Telling the round trip from the page's own time (for example against the fastest acknowledgement seen) would fix that; it is not built.

### In the site

`web.Config.Hybrid` (off unless the harness sets it), with `AckWindow`, `AckAfterPaint` and `AckRelease`:

- **reopen**: the page is served as it ships, with `data-newest-frame__reopen`. When the plugin reports the page behind, it sets `$_slow` and opens the stream again at the same URL, which then sends `Save-Data: on`. The stream handler gives such a stream `Spec.Window` instead of `Spec.Slow`, so the page gets an ack-paced stream instead of the 4 fps slow stream. A page that measured a slow link as it started also sends `Save-Data: on`, and would get the paced stream too; the measurements here do not include such a page.
- **inplace**: the page has `data-newest-frame` without `__reopen`, and `<body>` has `data-on:newest-frame-behind="@post('/pace?tab=' + $_tab, {requestCancellation: 'disabled'})"`. `POST /pace` calls `Hub.Pace(tab, AckWindow)`. Every stream has `Spec.Ack`, so it can be paced, but none starts paced. The stream stays open: no frame is lost or sent twice in the switch, and the page keeps its signals and the plugin its state. The listener is on `<body>` because a `@post` from the stream's own element would, with Datastar's default `requestCancellation`, abort the stream's request. Without `__reopen` the plugin reports again every 3 s while the page is behind, and the page posts again; `Pace` ignores a stream paced already.

After a release, the two differ. In **inplace** the plugin goes on watching, and if the page falls behind again it reports and posts again. In **reopen** the plugin has set `$_slow` and fired its one reopen, so it never asks again: a released stream stays unpaced for good, with the plugin dropping frames in the page.

At W=1, a paced stream is long polling: each acknowledgement asks for the next frame, so the page gets at most one frame per round trip plus the time to apply it. It differs from ordinary polling in three ways. Nothing goes out while the view does not change. The frame arrives over the open SSE stream, not as the response to the acknowledgement (a 204). And the server always sends the newest state, never a queue. A larger W works like TCP's sliding window: more frames in flight hide the round trip, and the server notices a slow page later. For a page that keeps up, plain push needs no requests and has no round-trip cap, so the hybrid pushes by default and switches to long polling only while a page is behind.

## Slow CPU on a fast link

15x CPU, Caddy, HTTP/2, 150 s, 3 runs each, the configurations in turn within a round. Median of the three runs, range in brackets. "Switch at" is when the page posted `/pace` or opened its stream with `Save-Data: on`. "Around the switch" is the 10 s from 5 s before to 5 s after; since the switch comes about 2 s after the page loads, that is the run's first 7 s.

| Configuration | Lag median (s) | Lag max (s) | Switch at (s) | Lag around switch, median / max (s) | Applied/s | Painted/s | Site wrote KB/s | Acks/s | Site CPU | Caddy CPU | Load |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Hybrid, reopen | 0.31 (0.30-0.35) | 0.77 (0.70-0.80) | 2.1 (2.0-2.2) | 0.37 / 0.77 | 2.49 | 2.48 | 2.11 (2.10-2.17) | 2.45 | 1.8% | 0.2% | 1.8 (1.4-3.0) |
| Hybrid, inplace | 0.34 (0.30-0.37) | 0.70 (0.67-0.75) | 2.2 (2.1-2.2) | 0.37 / 0.67 | 2.41 | 2.40 | 2.19 (2.15-2.26) | 2.37 | 1.9% | 0.2% | 1.7 (1.6-2.0) |
| Plugin, `__reopen` | 0.62 (0.61-0.64) | 1.21 (1.13-1.24) | 2.2 | 0.70 / 0.81 | 2.76 | 2.75 | 3.36 (3.28-3.38) | 0 | 1.8% | 0.1% | 1.7 (1.4-1.9) |
| Plugin, no reopen | 0.28 (0.27-0.32) | 1.03 (0.96-1.17) | never | | 2.61 | 2.60 | 7.01 (6.77-7.16) | 0 | 1.9% | 0.2% | 2.1 (1.6-2.1) |
| Ack W=1 after paint, no plugin | 0.31 (0.31-0.34) | 0.72 (0.72-0.85) | from the start | | 2.52 | 2.51 | 1.93 (1.92-1.97) | 2.52 | 1.9% | 0.2% | 1.9 (1.4-1.9) |

`python3 -I table.py runs slow-hybrid-reopen slow-hybrid-inplace slow-plugin-reopen slow-plugin-noreopen slow-ack1paint` prints it with every range.

Both hybrids reach the lag of ack pacing from the start: 0.31 to 0.34 s at the median, 0.70 to 0.77 s at most, against 0.62 and 1.21 s for the plugin's slow stream. The page asks 2.1 to 2.2 s into every run (the harness starts its clock once the page is live), when the plugin has the 2.7 s of history it needs before it judges. The hybrids write 0.2 KB/s more than ack pacing from the start, which is the 2 s of unpaced stream at 7 KB/s before the switch, spread over 150 s. The references reproduce `bench/drift`'s numbers of the day before within its ranges (plugin with `__reopen` 0.64 s then, 0.62 s now; ack after paint 0.33 then, 0.31 now). The site's and Caddy's CPU with one viewer cannot tell the configurations apart.

## Fast CPU on a far link

No throttling, Caddy, HTTP/2, the harness's delaying proxy (`LIFE_DRIFT_RTT`), 90 s, one run each.

| Configuration | RTT | Lag median (s) | Lag max (s) | Applied/s | Painted/s | Acks/s | Switch | Site wrote KB/s | Link up / down KB/s | Site CPU | Caddy CPU | Load |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Hybrid, reopen | 100 ms | 0.05 | 0.06 | 10.07 | 10.07 | 0 | never | 6.68 | 0.08 / 8.14 | 3.7% | 0.2% | 1.5 |
| Hybrid, inplace | 100 ms | 0.04 | 0.06 | 10.07 | 10.07 | 0 | never | 7.08 | 0.08 / 8.54 | 4.1% | 0.2% | 1.7 |
| Plugin, `__reopen` | 100 ms | 0.00 | 0.08 | 10.08 | 10.08 | 0 | never | 6.53 | 0.08 / 7.99 | 3.6% | 0.2% | 3.2 |
| Ack W=1 after paint | 100 ms | 0.07 | 0.17 | 6.63 | 6.63 | 6.63 | from the start | 4.85 | 0.91 / 9.31 | 7.6% | 1.6% | 1.8 |
| Hybrid, reopen | 300 ms | 0.14 | 0.15 | 10.06 | 10.06 | 0 | never | 6.78 | 0.08 / 8.23 | 4.3% | 0.2% | 2.2 |
| Hybrid, inplace | 300 ms | 0.09 | 0.10 | 10.07 | 10.07 | 0 | never | 7.09 | 0.08 / 8.55 | 3.6% | 0.2% | 1.6 |
| Plugin, `__reopen` | 300 ms | 0.11 | 0.12 | 10.07 | 10.06 | 0 | never | 6.96 | 0.08 / 8.42 | 4.2% | 0.2% | 1.8 |
| Ack W=1 after paint | 300 ms | 0.28 | 0.49 | 2.82 | 2.82 | 2.82 | from the start | 2.25 | 0.42 / 5.65 | 5.7% | 0.7% | 1.6 |

The hybrid never switched on a far link: no `/pace`, no `Save-Data` stream, no acknowledgement, and the page applied 10.07 frames a second at both round trips, as with the plugin alone. Ack pacing from the start capped it at 6.6 and 2.8 frames a second, as in `bench/drift`.

## A page slow only for a while

15x CPU for 60 s, then 1x through CDP (at 60.1 to 60.4 s), 120 s in all, one run each; in the `relapse` runs 15x again from 120 s to 180 s. `python3 -I table.py phases 60 phase-…` and `phases 60,120 relapse-…`.

| Run | Phase (s) | Lag median (s) | Lag max (s) | Applied/s | Painted/s | Site wrote KB/s | Acks/s | Site CPU | Caddy CPU | Events |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Hybrid, reopen | 0-60 (15x) | 0.33 | 0.82 | 2.50 | 2.48 | 2.52 | 2.42 | 1.8% | 0.17% | `Save-Data` stream at 2.1 s; released between 65 and 66 s |
| | 60-120 (1x) | 0.00 | 0.00 | 10.05 | 10.05 | 6.09 | 0.88 | 5.4% | 0.63% | |
| Hybrid, inplace | 0-60 (15x) | 0.32 | 0.71 | 2.43 | 2.42 | 2.56 | 2.35 | 1.8% | 0.15% | `/pace` at 2.1 s; released between 65 and 66 s |
| | 60-120 (1x) | 0.00 | 0.02 | 10.03 | 10.03 | 6.95 | 0.88 | 5.2% | 0.57% | |
| Plugin, `__reopen` | 0-60 (15x) | 0.61 | 1.11 | 2.85 | 2.83 | 3.51 | 0 | 1.7% | 0.07% | `Save-Data` stream at 2.2 s |
| | 60-120 (1x) | 0.30 | 0.51 | 4.00 | 4.00 | 2.85 | 0 | 4.5% | 0.23% | |
| Relapse, hybrid reopen | 0-60 (15x) | 0.31 | 0.72 | 2.43 | 2.42 | 2.57 | 2.33 | 1.8% | 0.17% | `Save-Data` stream at 2.2 s; released between 65 and 66 s |
| | 60-120 (1x) | 0.00 | 0.02 | 10.05 | 10.05 | 7.46 | 0.87 | 5.6% | 0.58% | |
| | 120-180 (15x) | 0.31 | 0.81 | 2.57 | 2.57 | 7.04 | 0 | 2.0% | 0.15% | never paced again |
| Relapse, hybrid inplace | 0-60 (15x) | 0.29 | 0.71 | 2.38 | 2.37 | 2.66 | 2.30 | 1.9% | 0.17% | `/pace` at 2.2 s; released between 65 and 66 s |
| | 60-120 (1x) | 0.00 | 0.01 | 10.03 | 10.03 | 7.02 | 0.88 | 5.6% | 0.55% | |
| | 120-180 (15x) | 0.35 | 0.80 | 2.43 | 2.43 | 2.24 | 2.35 | 2.0% | 0.18% | `/pace` again at 122.1 s, paced |
| Relapse, plugin `__reopen` | 0-60 (15x) | 0.71 | 1.01 | 2.67 | 2.65 | 3.64 | 0 | 1.7% | 0.07% | `Save-Data` stream at 2.2 s |
| | 60-120 (1x) | 0.31 | 0.42 | 4.02 | 4.02 | 3.17 | 0 | 5.0% | 0.25% | |
| | 120-180 (15x) | 0.62 | 0.91 | 2.77 | 2.77 | 3.03 | 0 | 1.9% | 0.07% | |

Both hybrids switched back between 65 and 66 s, 5 to 6 s after the CPU sped up: the 5 s of `Release` from the first acknowledgement that came early. While still paced at 1x the page already applied 10 frames a second, since on this link it acknowledges within a few tens of milliseconds; the 0.88 acknowledgements a second of the 1x phase are those 5 s. The plugin's slow stream stays at 4 frames a second for good once the page is fast again, 0.30 s behind.

When the page slowed down again at 120 s, the inplace hybrid posted `/pace` 2.1 s later and was paced as at the start. The reopen hybrid could not ask again: it stayed on the unpaced stream at 7 KB/s with the plugin dropping frames in the page, which is the "plugin, no reopen" row of the first table (lag 0.31 s, no worse, but four times the bytes).

### At the edge of keeping up

Inplace hybrid at 2x to 6x CPU, 150 s, one run each, to see whether a page near the edge switches back and forth.

| CPU | Lag median (s) | Lag max (s) | Applied/s | Site wrote KB/s | Acks/s | Paced, released | Load |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 2x | 0.00 | 0.00 | 10.07 | 6.75 | 0 | never | 1.4 |
| 3x | 0.00 | 0.04 | 10.07 | 6.78 | 0 | never | 1.9 |
| 4x | 0.01 | 0.12 | 9.83 | 7.00 | 0 | never | 1.6 |
| 5x | 0.01 | 0.20 | 9.11 | 6.76 | 0 | never | 1.9 |
| 6x | 0.10 | 0.22 | 6.50 | 4.51 | 6.41 | paced at 2.5 s, never released | 1.6 |

No run flapped. Up to 5x the page dropped too few frames to report; at 6x it reported after 2.5 s and stayed paced at 6.5 frames a second, its acknowledgements late, so the release never came. This is the gap between the two rules described above. A page whose own speed swings every few seconds could still switch back and forth; the doubling bounds that to a switch per 40 s after three releases. Not measured.

## Firefox

Firefox cannot slow its CPU, so each board frame costs the page 150 ms more (`LIFE_DRIFT_COST`), Caddy, HTTP/2, 150 s, one run each.

| Configuration | Lag median (s) | Lag max (s) | Switch at (s) | Applied/s | Painted/s | Site wrote KB/s | Acks/s | Site CPU | Caddy CPU | Load |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Hybrid, inplace | 0.28 | 0.39 | 2.7 | 4.12 | 4.11 | 3.21 | 4.05 | 2.2% | 0.3% | 2.6 |
| Hybrid, reopen | 0.28 | 0.48 | 2.7 | 4.12 | 4.11 | 3.37 | 4.04 | 2.2% | 0.3% | 1.6 |
| Plugin, `__reopen` | 0.45 | 0.76 | 2.7 | 4.00 | 3.99 | 3.09 | 0 | 1.9% | 0.1% | 1.6 |

The hybrid works the same in Firefox: the page asked after 2.7 s, was paced, and stayed 0.28 s behind at the median against 0.45 s on the slow stream. A page with a fixed cost of 150 ms a frame manages about 4 frames a second, close to the slow stream's 4, so here the slow stream sends about as many bytes as pacing.

## The trace

`trace-hybrid-inplace.trace.json.gz` (75 MB unzipped): the inplace hybrid, the page at full speed from load, slowed 15 times at 12.0 s, the trace from 5 s to 25 s into the run. The page posted `/pace` at 14.3 s. `trace-hybrid-reopen.trace.json.gz` is the same for the reopen hybrid, which opened its `Save-Data` stream at 14.5 s. `switch.py` on the inplace trace, seconds from the plugin's `newest-frame-behind` (a `lith:behind` mark the harness adds, 9.31 s into the trace):

| s from switch | Main thread busy | Longest task (ms) | Frames applied | Stream reads (KB) | `/ack` | `/pace` |
| --- | --- | --- | --- | --- | --- | --- |
| -5 | 42% | 20 | 10 | 10 (464) | 0 | 0 |
| -4 | 42% | 20 | 10 | 10 (464) | 0 | 0 |
| -3 | 73% | 197 | 8 | 8 (371) | 0 | 0 |
| -2 | 106% | 246 | 3 | 3 (510) | 0 | 0 |
| -1 | 90% | 247 | 2 | 3 (417) | 0 | 1 |
| 0 | 107% | 237 | 2 | 2 (186) | 1 | 0 |
| 1 | 105% | 407 | 2 | 2 (93) | 2 | 0 |
| 2 | 99% | 389 | 1 | 1 (47) | 1 | 0 |
| 3 to 9 | 84 to 109% | 212 to 277 | 2 | 2 (93) | 2 | 0 |

Busy share is per second by the start of each task, so it can pass 100%. Stream reads are the bytes Datastar reads from the stream, decompressed (the `lith:read` marks).

What it shows: at full speed the page reads one frame of about 46 KB per read, ten a second, and the main thread is 42 to 50% busy. Within a second of the slowdown the plugin is dropping frames: the page still reads all ten frames a second (about 460 KB) in three reads, but applies two or three. The `/pace` request goes out in the same task as the plugin's event (it shows in the second before the mark because the page's listener on `<body>` runs before the harness's on `document`). From the next write on, each read holds exactly one frame of 47 KB, two a second, each followed by one `/ack`: the page reads a fifth of the bytes and applies as many frames as before. The main thread stays fully busy, half applying frames and half rendering them, as `bench/drift` found for every configuration at 15x: pacing changes what the page receives, not what applying a frame costs it. In the reopen trace the second after the switch has one longer task (445 to 527 ms) where the new stream's element and first frame come in; otherwise the two traces look alike.

Screenshot: `flame-switch-inplace-8s.png`, Chrome DevTools' Performance panel (`TestFlameShot`) on `flame-switch-inplace-8s.trace.json.gz`, the 8 s from 5.5 to 13.5 s into the trace (`crop.py` from `bench/drift`). Until about 1.5 s the main thread runs short tasks at full speed; from there, 15x, each task applies one frame and is followed by a rendering task. The Network track shows `pace` starting at 3.8 s and the `ack` requests after it, one per frame. A request's bar there runs until the page's main thread handles its response, which on a page this busy is several hundred milliseconds after the server answered.

## Many viewers

`TestHybridLoad` in `live` (no browsers): one Game of Life view of 80 by 60 cells in rows of short tags, as on the site (about 48 KB of HTML a frame, about 1.7 KB on the wire with zstd, against 0.7 KB for the site's board), 10 frames a second, N zstd streams whose pages take every write at once. A tenth of the pages are slow: they acknowledge a write 400 ms after it goes out, the fast ones after 20 ms. "Plugin" sends every stream every frame; "ack" paces every stream from the start (W=1); "hybrid" paces the slow pages 3 s after they open, as `newest-frame-behind` would. Acknowledgements call `Hub.Ack` directly ("direct"), or go through an HTTP POST to a server in the same process ("HTTP"), whose client side then counts in the CPU too. 5 s warm-up, 20 s measured, three rounds with the modes in turn, median and range. CPU is the whole test process, in percent of one core, GOMAXPROCS 20.

| Streams | Mode | CPU, acks direct | CPU, acks over HTTP | Acks/s | Wire KB/s | Frames sent/s |
| --- | --- | --- | --- | --- | --- | --- |
| 200 | Plugin | 49% (37-49) | 50% (50-51) | 0 | 3,375 | 1,990 |
| 200 | Ack, every stream | 49% (44-51) | 97% (97-100) | 1,841 | 3,117 | 1,841 |
| 200 | Hybrid | 48% (47-48) | 50% (48-51) | 50 | 3,124 | 1,841 |
| 1,000 | Plugin | 172% (155-182) | 199% (171-201) | 0 | 16,876 | 9,950 |
| 1,000 | Ack, every stream | 172% (155-206) | 452% (417-467) | 9,200 | 15,566 | 9,200 |
| 1,000 | Hybrid | 163% (158-172) | 186% (166-204) | 250 | 15,600 | 9,200 |

Raw output: `load-direct.log`, `load-http.log` (commit `3ba0911`, load average 2.5 and 2.0 before). Wire bytes and frames are the same in both runs; the table has the HTTP run's.

With acknowledgements straight to the hub, the three modes cannot be told apart: the spread between rounds of one mode (up to 50% of a core at 1,000 streams) is larger than any difference between them. What the hub does per acknowledgement is too little to see next to compressing 10,000 frames a second.

Through HTTP the cost shows: pacing every stream took 97% of a core against 50% at 200 streams, and 452% against 199% at 1,000, that is about 0.25 ms of CPU per acknowledgement for both sides of an HTTP/1.1 request without TLS in one Go process. `bench/drift` put the site's and Caddy's share at 1 to 3 ms per acknowledgement with TLS and HTTP/2, from single runs. Either way it grows with every viewer that keeps up. The hybrid's 250 acknowledgements a second at 1,000 streams come from the slow hundred only, and its CPU stayed within the plugin's range. It sent 7.5% fewer bytes than the plugin, since the slow tenth got a quarter of the frames. Renders do not change: one view, rendered once per tick for everyone, and a fast viewer always kept up, so no render was deferred.

The load test leaves out the network, Caddy, TLS and the browsers' side, and the synthetic board compresses to 1.7 KB a frame where the site's comes to 0.7 KB. It answers whether pacing everyone costs more than pacing the slow ones (yes, by one request per frame per fast viewer), not how many viewers a machine holds.

## Conclusion

- **Slow CPU on a fast link.** The hybrid is as good as ack pacing from the start: 0.31 to 0.34 s behind at the median and 0.70 to 0.77 s at most at 15x, against 0.62 and 1.21 s for the plugin's slow stream, the configuration the site ships. It writes 2.1 to 2.2 KB/s against 3.4 for the slow stream and 7.0 for the plugin without reopen. The switch comes about 2 s into the run, when the plugin has seen enough frames to judge.
- **Fast CPU on a far link.** It costs nothing: no switch, no acknowledgement, 10 frames a second at 100 and 300 ms, where ack pacing from the start gets 6.6 and 2.8.
- **A page slow for a while.** Both variants switch back 5 to 6 s after the page is fast again and return to 10 frames a second; the plugin's slow stream stays at 4 for good. Only inplace can pace the stream again when the page slows down a second time (after 2.1 s here). No run flapped: switching on needs a page that applies fewer than about half the frames, switching off one that applies all of them within a tick, and each release doubles the wait for the next.
- **Many viewers.** Only the slow viewers send requests. With 1,000 viewers of whom 100 are slow, that is 250 acknowledgements a second instead of 9,200, and the CPU of the in-process load test stayed within the plugin's range, where pacing everyone took 2.3 times as much once acknowledgements went through HTTP.
- **What it does not change.** The slow page's main thread stays fully busy, half applying frames and half rendering them, as with every configuration in `bench/drift`. Pacing changes what the page receives, not what a frame costs it.

The hybrid is worth making a lith option, in the inplace variant. The reopen variant performs the same while the page is slow, but it reuses `Save-Data: on`, which a page on a slow link also sends, so a slow link would get a stream capped at one frame per round trip instead of the 4 fps slow stream; it cannot ask again after a release; and the reopen costs a new request, new first frames and a longer task in the page. Inplace keeps the stream, its compression state and the page's state, and needs no reopen.

What it costs: every stream needs `Spec.Ack` and `Spec.Release`, the app two routes (`/ack`, `/pace`) and one attribute on `<body>`, and the page the plugin without `__reopen`. The prototype's routes check only the tab id and the stream's token: a real option needs the session check and a rate limit on both. A paced page on a link with a round trip over one tick (100 ms here) never meets the release rule, so it stays at about one frame per round trip until it reloads; the rule would have to allow for the round trip. And for the first 2 s a slow page gets the full stream, as it does today.
