# What a leaner frame buys a slow page

The point made on the Datastar Discord: the Game of Life falls behind on a slow CPU because each frame is 46 KB of HTML with 4,000 elements, and a server that sent a leaner frame would not need `newest-frame.js` or ack pacing. This file measures that: the same board in four markups, at 15x CPU and at the slowdown where each one falls behind, with traces of where a frame's time goes. A second part does the same for a frame that cannot be made as cheap, an 80 by 24 terminal whose every cell changes.

## Setup

- Machine: tower-dev, Intel Core i5-13500 (20 threads), 62 GB, Linux 6.18.38 (Unraid). Another agent ran browser measurements on the same machine in the same hours; all runs of both took `/tmp/lith-bench.lock` (`flock`), so no two browsers measured at once. The load average when each run took the lock is the first line of its `.log` and the last column of the tables. Five of the 24 comparison runs started at a load of 3.4 to 9.1 (`cmp-lean-2`, `cmp-svg-1`, `cmp-svg-nostats-1` and `-2`, `cmp-canvas-2`); their numbers sit inside the range of the other runs of the same markup, so none was repeated.
- Software: Go 1.27.2, headless Chromium 154.0.8037.57, Caddy 2.10.2 (TLS, HTTP/2, `flush_interval -1`), Datastar 1.0.4 (Starbase's patched build), `newest-frame.js` at commit fd92fa12 of datastar-attribute-newest-frame.
- Date: 2026-10-17, 08:15 to 14:45 UTC. The container on tower-dev restarted twice, at about 13:04 and 13:12; the run in progress at the second restart (`fix-term-15x-ack1paint`) was run again from the start, and no other run was cut off.
- Code: branch `frame-cost`, off `ack-pacing` at 0a915ee (the harness of `bench/drift`). The runs used the working tree that is committed as c45d60f; their logs say `0a915ee+dirty`, and the runs after that commit say `c45d60f`. The terminal's CSS changed once on the way (its font was too large, so a third of it was clipped); every terminal run in this file came after that change.
- Harness: `TestLifeDrift` as in `bench/drift/RESULTS.md`: the site's Game of Life, 80 by 50 cells, ten generations a second, a fresh soup every 15 s, behind Caddy over HTTP/2, CPU slowed with `Emulation.setCPUThrottlingRate`, without the plugin unless a row says so. New switches, off unless set, read only by the test:
  - `LIFE_DRIFT_RENDER` serves the demo with `life.Markup{Board: ...}.Demo()` (`site/internal/demos/life/markup.go`) in place of `life.Demo`: `cells`, `lean`, `svg`, `canvas`, `term`, `termruns` (below).
  - `LIFE_DRIFT_STATS=off` leaves the counters, the population sparkline, the colour bar and the status line out of the frames. The page still shows them as they were when it loaded.
  - `LIFE_DRIFT_SHOT=file.png` takes a picture of the board at the end (`smoke/*.png`).
- Lag, frames applied, main thread busy: as in `bench/drift`. The page notes the generation of each board frame it applies (the board's `data-gen` in the new markups, the first counter in the site's own), and lag is how long ago the board passed it. Frames shown come from the traces: a frame counts as shown when the main thread commits a new picture to the compositor after it and before the next frame is applied.
- Raw outputs in this directory: `NAME.log`, `NAME.csv`, `NAME.lag.csv`, traces as `NAME.trace.json` or, over 20 MB, `.trace.json.gz`. `run.sh` runs one measurement under the lock, the `batch-*.sh` scripts ran them all, `table.py` makes the run tables (`table-compare.md`), `framecost.py` splits a trace into the cost of one frame (`framecost.md`), `anatomy.txt` is the output of `TestFrameAnatomy`, and `../drift/tracesummary.py` and `../drift/crop.py` are reused (`traces-summary.txt`).

## The markups

All four draw the same cells from the same board state, with the same colours, the same dots for dead cells, and the same three sizes for live, dying and trailing cells (pictures at full speed in `smoke/cells.png`, `lean.png`, `svg.png`, `canvas.png`).

- **cells**, the site as it is: `<div id="life-board">` with 50 rows `<div id="life-rN">` of 80 `<p class=X>` each, one class letter per cell (`_` for a dead cell), every cell the same length so that zstd finds each frame in the one before.
- **lean**, the same grid with the least markup a Datastar user would write: 4,000 `<p>` straight in the board, no rows and no ids but the board's, and a class only on a cell that is not plainly dead (`<p>` against `<p class=_>`).
- **svg**, one `<svg>` with one `<path>` per class letter that occurs (live, dying and trailing per colour, so 2 to 6 on this board): each cell is `M x y h0`, a line of zero length whose square cap of 0.86, 0.65 or 0.45 is the cell. The dead cells' dots are an SVG `<pattern>` of the same size and colour. As in the plugin's own demo, but with the colours and the fading cells kept.
- **canvas**, no element morph: each frame is a signal patch `{"_lifeb": {"g": generation, "c": "4,000 letters"}}`, and a `data-effect` on a `<canvas>` draws it with `fillRect`, reading each letter's colour once from CSS custom properties.

Where they differ from the site as it ships:

- The site turns the board on its side in a narrow stage (CSS grid, `grid-auto-flow: column`). svg and canvas do not; that would take a second `viewBox` or a transform. The harness's page is 1440 pixels wide, so it never showed.
- The site's smooth style (`data-lith-style="smooth"`) draws round cells; svg and canvas draw squares, the default style. svg would need `stroke-linecap: round` under that style, canvas an `arc`.
- Drawing on the board looks up whether a cell is alive in the DOM (`#life-rN` row, then the cell's class). lean, svg and canvas break that lookup; it would have to read the board from a signal or the path. Drawing was not measured.
- The canvas effect reads the colours again only when the canvas changes size, so it would miss a switch between light and dark until then.
- The canvas is sized in device pixels from `devicePixelRatio`; its squares are snapped differently at the edges than CSS backgrounds.

## Anatomy of a frame

What each markup sends, over 600 generations with a new soup every 150 (`TestFrameAnatomy`, `anatomy.txt`; zstd at its fastest level with a 2^19 window, one stream, flushed after each frame, as lith sends them). The on-the-wire bytes from the browser runs, below, agree within a few percent.

| Markup | Counters in the frame | Decompressed bytes | Elements | Attributes | zstd bytes |
| --- | --- | --- | --- | --- | --- |
| cells (the site) | yes | 46,450 | 4,077 | 4,088 | 709 |
| cells | no | 45,368 | 4,051 | 4,054 | 685 |
| lean | yes | 20,623 | 4,027 | 961 | 1,810 |
| lean | no | 19,526 | 4,001 | 926 | 1,777 |
| svg | yes | 8,718 | 34 | 59 | 1,357 |
| svg | no | 7,622 | 8 | 24 | 1,336 |
| canvas | yes | 5,217 | 26 | 35 | 870 |
| canvas | no | 4,072 | 0 | 0 | 834 |

The counters add about 1,100 bytes and 26 elements, the sparkline among them, whose `values` attribute changes once a second.

Leaner HTML is not fewer bytes on the wire. The site's cells are all ten bytes long, so a frame lines up with the one before and zstd sends little more than what changed: 709 bytes. lean is less than half the HTML and two and a half times the bytes on the wire, because each birth or death shifts the rest of the row (render.go's comment on `cellTags` says as much). svg's paths shift in the same way. canvas sends 834 bytes, a little more than the site's cells.

Where one frame's time goes, from 20 s traces (`framecost.md`). Milliseconds of the main thread per frame applied; "apply" is the `lith:apply board` measure, Datastar applying the event and what runs synchronously in it.

| Trace | Apply | of it: parse HTML | of it: script (morph or effect) | Datastar's MutationObserver | Style | Layout | Paint | Pre-paint, layerize, commit | rAF (sparkline) | GC | Other | Busy per frame | Frames/s | Shown/s |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| cells, 1x | 19.9 | 5.6 | 14.2 | 2.1 | 2.7 | 7.3 | 6.5 | 5.6 | 0.6 | 0.5 | 5.9 | 50.4 | 10.05 | 10.05 |
| cells, 15x, plugin (one frame per task) | 150.6 | 19.6 | 130.1 | 32.9 | 3.8 | 94.4 | 80.9 | 16.5 | 50.2 | 9.1 | 57.1 | 439.8 | 2.20 | 2.20 |
| cells, 15x, no plugin | 138.2 | 20.1 | 116.1 | 1.2 | 0.1 | 3.3 | 1.5 | 0.3 | 3.4 | 2.2 | 3.5 | n/a | 6.51 | 0.10 |

The second row is `trace-b-noreopen` from `/develop/_lith/lith-drift-traces/bench/drift/` (yesterday's trace with the plugin, no reopen): with one frame per task, each frame pays its own style, layout and paint, which is what a page that keeps up pays. Without the plugin (third row) the page applies 45 frames in one 7 s task and renders once after it, so the rendering per frame looks small; the trace also ends inside such a task, so its busy time is not counted and the column says n/a.

So, for the site's frame:

- At full speed a frame costs about 50 ms of the main thread: 20 ms to apply (6 ms parsing the HTML, 14 ms morphing), 2 ms in Datastar's MutationObserver, 22 ms of style, layout, paint and compositing, 6 ms of other work (reading the stream and parsing its events, the harness's own console message). Ten a second is half the main thread.
- At 15x a frame costs about 440 ms when it is rendered: 150 ms to apply (20 ms parse, 130 ms morph), 33 ms MutationObserver, 95 ms layout, 81 ms paint (each cell's background is a radial gradient: Chromium paints 4,000 of them per frame, `PaintImage`), 17 ms compositing, 50 ms in the sparkline's `requestAnimationFrame` callback, 57 ms other. The morph the Discord pointed at is a third of it.
- The counters and the sparkline: at full speed they add 13 ms to the cells frame (50.4 against 37.4 ms without them, `trace-cells-nostats-1x`), 7 ms to svg and 6 ms to canvas; at 15x, 28 ms to svg and 20 ms to canvas (below). That is as much as the svg board itself.

The same split for the four markups, with and without the counters in the frames, at full speed and at 15x:

| Markup, CPU | Counters | Apply | of it: parse HTML | of it: script | MutationObserver | Style | Layout | Paint | Compositing | rAF | GC | Other | Busy per frame | Shown/s |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| cells, 1x | yes | 19.9 | 5.6 | 14.2 | 2.1 | 2.7 | 7.3 | 6.5 | 5.6 | 0.6 | 0.5 | 5.9 | 50.4 | 10.05 |
| cells, 1x | no | 15.5 | 3.9 | 11.6 | 2.1 | 7.5 | 0.0 | 5.1 | 2.3 | 0.0 | 0.5 | 4.5 | 37.4 | 10.06 |
| lean, 1x | yes | 14.3 | 3.1 | 11.1 | 2.2 | 3.4 | 9.0 | 6.7 | 7.0 | 0.7 | 0.5 | 6.3 | 49.1 | 10.06 |
| lean, 1x | no | 14.0 | 3.0 | 11.0 | 2.4 | 8.5 | 0.0 | 5.1 | 2.5 | 0.0 | 0.5 | 5.4 | 38.1 | 10.10 |
| svg, 1x | yes | 3.3 | 0.9 | 2.4 | 2.8 | 5.5 | 4.2 | 2.6 | 4.9 | 0.3 | 0.4 | 5.5 | 29.2 | 10.05 |
| svg, 1x | no | 2.4 | 0.8 | 1.6 | 3.8 | 6.1 | 0.2 | 0.3 | 4.2 | 0.0 | 0.3 | 5.1 | 22.2 | 10.10 |
| canvas, 1x | yes | 2.1 | 0.0 | 2.1 | 1.7 | 4.1 | 2.9 | 2.1 | 5.4 | 0.2 | 0.2 | 8.2 | 26.7 | 10.05 |
| canvas, 1x | no | 3.1 | 0.0 | 3.0 | 0.0 | 4.8 | 0.0 | 0.0 | 5.2 | 0.0 | 0.2 | 7.2 | 20.5 | 10.10 |
| cells, 15x | yes | 138.2 | 20.1 | 116.1 | 1.2 | 0.1 | 3.3 | 1.5 | 0.3 | 3.4 | 2.2 | 3.5 | n/a | 0.10 |
| cells, 15x | no | 140.3 | 19.8 | 119.3 | 1.5 | 5.2 | 0.0 | 1.7 | 0.3 | 0.0 | 1.7 | 9.4 | n/a | 0.10 |
| lean, 15x | yes | 116.9 | 15.7 | 99.4 | 1.2 | 0.1 | 4.6 | 1.1 | 0.3 | 4.8 | 2.1 | 6.7 | n/a | 0.10 |
| lean, 15x | no | 110.1 | 15.8 | 92.5 | 1.1 | 5.6 | 0.0 | 1.6 | 0.3 | 0.0 | 2.4 | 6.5 | n/a | 0.10 |
| svg, 15x | yes | 12.6 | 3.2 | 9.3 | 11.7 | 13.9 | 13.3 | 7.3 | 14.6 | 1.5 | 1.1 | 19.0 | 92.9 | 10.00 |
| svg, 15x | no | 7.8 | 2.6 | 5.2 | 10.6 | 14.8 | 1.0 | 1.3 | 13.0 | 0.0 | 0.7 | 15.7 | 64.8 | 10.00 |
| canvas, 15x | yes | 20.4 | 0.0 | 12.5 | 10.7 | 4.9 | 7.6 | 3.6 | 9.7 | 1.0 | 0.6 | 39.7 | 96.8 | 9.90 |
| canvas, 15x | no | 12.5 | 0.0 | 12.0 | 0.0 | 13.1 | 0.0 | 0.0 | 18.1 | 0.0 | 0.6 | 32.3 | 76.5 | 10.00 |

"Compositing" is pre-paint, layerize, commit and layer updates. For canvas at 15x with the counters, 7.7 ms of the apply is style and layout forced inside it. cells and lean at 15x apply 40 to 80 frames per task and render once after it, and their traces end inside such a task, so their busy time per frame is not comparable (n/a); svg and canvas render each frame. Where "Layout" is 0.0 without the counters, nothing on the page changes size: the counters' text is what makes the page lay out again.

Reading it:

- lean saves little. It removes 3,000 attributes and the 50 row elements, but the page still parses and morphs 4,000 elements: 14 ms to apply at 1x (against 20), 110 to 117 ms at 15x (against 138). Its layout and paint per frame are the same as cells'. Datastar's morph cost follows the element count, not the bytes.
- svg cuts the apply from 20 ms to 3 ms at 1x and from about 140 to 8 to 13 ms at 15x, and paint, since there are no 4,000 gradient backgrounds. What remains at 15x is the rendering (style, layout, compositing, 35 to 40 ms), Datastar's MutationObserver (11 to 12 ms, it walks the changed `d` attributes) and a floor of other work (16 to 19 ms) that every markup pays per frame.
- canvas applies a frame in 2 to 3 ms at 1x and 12 to 20 ms at 15x, mostly the effect's 4,000 `fillRect` calls. Its "other" per frame at 15x, 32 to 40 ms, is the largest: the stream's reading and parsing of a 4 KB signal patch per frame, and Datastar merging it into the signal store.

## Comparison at 15x

15x CPU, Caddy, HTTP/2, no plugin, 150 s, 3 runs each; median of the runs, range in brackets. Frames shown per second from the 20 s trace of each (`trace-*-15x`), not from these runs. Decompressed bytes from the anatomy above.

| Markup | Counters | Lag median (s) | Lag max (s) | Frames applied/s | Frames shown/s (trace) | Main thread busy | Wire bytes/frame | Decompressed bytes/frame | Load |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| cells (the site) | yes | 28.71 (28.55-29.16) | 52.32 (51.95-53.16) | 6.77 | 0.10 | 100% | 709 | 46,450 | 1.9 |
| lean | yes | 14.24 (13.44-15.39) | 24.75 (23.13-24.90) | 8.60 | 0.10 | 100% | 1,756 | 20,623 | 2.0 |
| svg | yes | 0.00 | 0.00 | 10.09 | 10.0 | 85% (83-86) | 1,304 | 8,718 | 1.6 |
| canvas | yes | 0.00 (0.00-0.01) | 0.02 (0.01-0.05) | 10.11 | 9.9 | 81% | 802 | 5,217 | 2.5 |
| cells | no | 24.35 (24.16-24.54) | 44.34 (43.95-44.48) | 7.29 | 0.10 | 100% | 675 | 45,368 | 1.7 |
| lean | no | 10.93 (10.76-11.34) | 16.14 (15.85-17.13) | 9.21 | 0.10 | 100% | 1,711 | 19,526 | 1.7 |
| svg | no | 0.00 | 0.00 (0.00-0.01) | 10.09 | 10.0 | 58% (56-58) | 1,319 | 7,622 | 4.3 |
| canvas | no | 0.00 | 0.00 | 10.11 | 10.0 | 53% (50-56) | 789 | 4,072 | 2.0 |

`table-compare.md` has every column with its range (site and Caddy CPU, KB/s). The site's CPU was 1.9 to 2.2% of a core in every row.

At 15x, svg and canvas keep up without any fix: every frame applied and shown, lag under 50 ms. cells falls 52 s behind in 150 s as yesterday, lean half as far. Taking the counters out of the frames helps cells and lean a little and leaves them behind; for svg and canvas it frees a third of the main thread.

## Breaking points

The lowest slowdown at which each falls behind without a fix, 60 s runs (`break-*.log`). "Keeps up" means lag stays under about a second and does not grow; numbers are lag median / lag at 60 s in seconds. The rates in italics are runs between the asked steps, to place the break more closely.

| Markup | 4x | *6x* | *8x* | 10x | *12x* | 15x | 20x | 30x | *40x* | 50x | *80x* | *120x* | Breaks at |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| cells (the site) | 0.0 / 0.0 | 0.1 / 0.1 | 0.4 / 0.3 | 2.4 / 3.3, growing | | 12.8 / 22.0 | 20.2 / 34.5 | 27.9 / 47.3 | | 44.8 / 67.6 | | | 10x (8x keeps up) |
| lean | 0.0 / 0.0 | | | 0.8 / 0.4 | 3.1 / 3.5, growing | 7.9 / 12.9 | 15.9 / 26.8 | 25.7 / 42.7 | | 41.0 / 61.9 | | | 15x (12x already behind) |
| svg | 0.0 / 0.0 | | | 0.0 / 0.0 | | 0.0 / 0.0 | 0.0 / 0.0 | 0.0 / 0.0 | | 0.1 / 0.0 | 0.4 / 0.7 | 2.5 / 1.7, falling | not within 50x |
| canvas | 0.0 / 0.0 | | | 0.0 / 0.0 | | 0.0 / 0.0 | 0.0 / 0.0 | 0.7 / 0.2 | 8.1 / 13.0 | 14.5 / 24.7 | | | 50x (40x already behind) |

All with the counters in the frames; loads 1.3 to 2.4.

svg does not break within 50x, and at 120x its lag is 2 to 4 s from the page's start and falls during the run. It applies every frame at any of these rates, but it does not show every frame: at 50x the page painted 2.9 frames a second, a paint every 271 ms at the median with up to 9 frames applied between two (`trace-svg-50x`, `traces-summary.txt`). Once a frame is cheap to apply, Chromium itself skips the renders the main thread has no time for, and the page drops the frames it cannot show without any plugin. With the site's markup it cannot do that, because the morph alone takes longer than the time between two frames.

## The plugin where a lean frame breaks

The leanest markup that falls behind within 50x is canvas, at 40x. At 50x, one 60 s run each:

| Configuration | Lag median (s) | Lag max (s) | Lag at 60 s (s) | Frames applied/s | Main thread busy | Site wrote KB/s | Load |
| --- | --- | --- | --- | --- | --- | --- | --- |
| no plugin | 14.50 | 24.70 | 24.7 | 6.75 | 98% | 9.74 | 1.8 |
| plugin, no reopen | 7.92 | 11.43 | 11.3 | 8.95 | 97% | 8.61 | 1.8 |
| plugin, `__reopen` | 0.55 | 0.76 | 0.4 | 4.33 | 73% | 4.24 | 2.2 |

Without reopen the plugin cannot help much: it drops only element patches that replace the same elements, and signal patches never, so every canvas frame is still applied. It halves the lag only because it drops the counters' element patches. With `__reopen` it asks for the slow stream at once (the log says "a slow stream (Save-Data) after 0.1 s"), the server sends four frames a second, and the page keeps up. So a developer who moves the board into signals to make it cheap also takes it out of the plugin's reach, and keeps only the reopen path, or ack pacing, which does not care what a write holds.

svg does not fall behind within 50x. At 120x, where it starts 4 s behind and takes the minute to come back to 2 s, the plugin without reopen brings it to 0.29 s median and 0.60 s max (`plugin-svg-120x-noreopen`, 2.33 frames applied a second, against 2.45 s and 4.45 s without it). So the plugin still helps an element-patched frame where even a lean one is too slow; it cannot help one sent as signals.

## A frame that cannot be made cheap: a terminal

The board can be drawn with a handful of elements because most of it is dots and a few colours. A terminal renderer's stress test cannot: every cell has its own character and colours, and every cell changes every frame. `LIFE_DRIFT_RENDER=term` replaces the board's frames with such a screen, 80 by 24, streamed through the same hub at the same ten frames a second: each line a run of tokens of 1 to 12 characters, each token with one of 16 text and 8 background colours (classes `fN bN`), all of it drawn anew from the generation number each frame. The counters are left out of these frames (`LIFE_DRIFT_STATS=off`) so that the frame is the terminal alone. Two markups (`smoke/term.png`, `smoke/termruns.png`):

- **term**: one `<span class="fN bN">` per character, in 24 row `<div>`s, as a renderer that styles each cell writes it.
- **termruns**: the cheapest honest markup for the same screen, one `<span>` per run of characters with the same colours.

| Markup | Decompressed bytes | Elements | Attributes | zstd bytes (anatomy) | Wire bytes/frame (runs) |
| --- | --- | --- | --- | --- | --- |
| term | 54,867 | 1,945 | 1,924 | 4,092 | 4,108 to 4,165 |
| termruns | 10,766 | 334 | 313 | 2,979 | 2,987 to 3,073 |

Both are an order of magnitude more on the wire than the board (700 bytes), since nothing repeats from frame to frame: 30 to 41 KB/s per viewer.

Where each falls behind without a fix, 60 s each (lag median / lag at 60 s):

| Markup | 4x | 10x | 15x | 20x | 30x | 50x | *80x* | Breaks at |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| term | 0.0 / 0.0 (100% busy) | 1.0 / 1.1, steady | 9.9 / 17.3 | 15.5 / 28.0 | 24.6 / 42.5 | | | 15x |
| termruns | 0.0 / 0.0 | 0.0 / 0.0 | 0.1 / 0.1 | 0.1 / 0.1 | 0.3 / 0.3 | 6.0 / 8.2 | 20.4 / 32.7 | 50x |

At 15x and at each one's break, 150 s each: no fix, `newest-frame.js` with `__reopen`, and ack pacing with a window of one acknowledged after the paint (`table-term.md`):

| Markup, CPU | Fix | Lag median (s) | Lag max (s) | Lag at 150 s (s) | Frames applied/s | Main thread busy | Site wrote KB/s | Acks/s | Load |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| term, 10x | none | 0.80 | 1.01 | 0.7 | 9.99 | 100% | 41.1 | 0 | 2.1 |
| term, 15x (break) | none | 20.31 | 37.51 | 37.5 | 7.61 | 100% | 41.4 | 0 | 2.0 |
| term, 15x | plugin, `__reopen` | 0.68 | 1.00 | 0.7 | 2.45 | 100% | 17.1 | 0 | 2.5 |
| term, 15x | ack pacing after paint | 0.29 | 0.50 | 0.5 | 2.46 | 99% | 10.1 | 2.46 | 2.2 |
| termruns, 15x | none | 0.04 | 0.06 | 0.0 | 10.03 | 100% | 29.9 | 0 | 0.9 |
| termruns, 15x | plugin, `__reopen` | 0.00 | 0.10 | 0.0 | 9.70 | 100% | 29.9 | 0 | 2.0 |
| termruns, 15x | ack pacing after paint | 0.05 | 0.16 | 0.0 | 7.47 | 96% | 22.3 | 7.47 | 1.6 |
| termruns, 50x (break) | none | 3.68 | 4.60 | 3.6 | 9.96 | 100% | 30.4 | 0 | 1.8 |
| termruns, 50x | plugin, `__reopen` | 0.63 | 1.04 | 0.4 | 2.75 | 100% | 12.6 | 0 | 2.3 |
| termruns, 50x | ack pacing after paint | 0.37 | 0.59 | 0.5 | 2.15 | 98% | 6.5 | 2.15 | 2.1 |

With `__reopen` the plugin asked for the slow stream 2.2 s into both break runs, and never at 15x for termruns, which kept up. termruns at 50x is at the edge: in the 150 s run it stayed 3.5 to 4.6 s behind without growing, in the 60 s run and in the trace run it was still growing at the end (8.2 s, 7.5 s at 50 s), and its trace shows the long tasks of a page that is behind: 47 frames per task, up to 75, and a paint every 7.5 s.

The traces at the break, without a fix: `trace-term-15x.trace.json` applies 34 to 39 frames per task from 2 MiB reads, 118 ms a frame (18 ms parsing, 102 ms morphing), and paints three times in 20 s; `trace-termruns-50x.trace.json` applies 88 ms a frame (13 ms parsing, 78 ms morphing), 47 frames per task, four paints. The screenshots, the same 8 s window as the others: `flame-term-15x-8s.png` (the 1,945-span terminal at 15x: tasks of 3 to 5 s that apply about 35 frames each, no paint between them) and `flame-termruns-50x-8s.png`.

So: the per-cell terminal behaves like the site's board, keeping up to 10x and falling behind from 15x. Merging runs of the same colour cuts the elements by six and lets it keep up to 30x, which covers any phone this test stands for; it breaks at 50x. Unlike the board, it cannot go further: the screen has no structure to draw as one path, and a canvas would draw 1,920 characters with `fillText`, which this test did not try. Where it breaks, both fixes bring it back within a second, ack pacing after paint a little closer (0.3 to 0.4 s against 0.6 to 0.7 s) with fewer bytes; where it keeps up, ack pacing costs a quarter of the frames (7.5 against 10 a second at 15x), since each frame waits for the acknowledgement of the one before.

## Traces and screenshots

20 s traces, from 30 s into a 55 s run, without the plugin: `trace-{cells,lean,svg,canvas}[-nostats]-{1x,15x}.trace.json[.gz]`, and `trace-svg-50x.trace.json`, `trace-term-15x.trace.json` and `trace-termruns-50x.trace.json`. They open in Chrome DevTools' Performance panel. Two screenshots of DevTools' Performance panel (`TestFlameShot`, the window cut with `../drift/crop.py`), the same 8 s, 6.5 to 14.5 s into each trace, as in `bench/drift`:

- `flame-cells-15x-8s.png`: the site's markup at 15x. One task of more than 7 s applies about 45 frames (the `lith:apply board` blocks in Timings), with no paint until it ends.
- `flame-svg-15x-8s.png`: svg at 15x, same window: short tasks, a frame applied and painted every 100 ms, the main thread about 85% busy with rendering (purple) as the largest share.
- `flame-canvas-15x-8s.png` is canvas in the same window, for completeness.
- `flame-term-15x-8s.png` and `flame-termruns-50x-8s.png`: the terminal at its break, described in its section.

## Conclusion

Does a leaner frame remove the need for `newest-frame.js` or ack pacing? For this demo, mostly yes, and the critics have a point; but not for the reason they gave, and not for every frame.

- **The site's frame breaks at 10x** (it keeps up at 8x, but uses the whole main thread from 6x on). The morph is a third of what a rendered frame costs at 15x (130 of 440 ms); the rest is layout, painting 4,000 gradient backgrounds, the sparkline, Datastar's MutationObserver and reading the stream.
- **Fewer bytes alone does nothing.** lean halves the HTML, keeps 4,000 elements, and breaks at 12x instead of 10x, while sending two and a half times the bytes on the wire. The cost follows the elements the morph walks and the browser lays out and paints, not the size of the HTML.
- **Fewer elements does.** The board as one SVG with a path per colour (8 to 34 elements) keeps up at 15x with the main thread 85% busy (58% without the counters), applies every frame up to 120x (where it is left 2 to 4 s behind from the page's start and catches up), and from about 50x lets Chromium skip the paints it has no time for (2.9 shown a second at 50x). The board as a signal drawn into a canvas keeps up to 30x and breaks at 40x. On a page this cheap, the browser does what the plugin does, by itself.
- **The rest of the page is not free.** The counters, the sparkline and the colour bar cost 7 to 13 ms a frame at full speed and 20 to 28 ms at 15x, as much as the SVG board. Sending them once a second instead of with every frame would be the next saving.
- **A frame that cannot be made cheap still needs a fix.** An 80 by 24 terminal with a span per character breaks at 15x like the board; merging runs of a colour gets it to 30x, and it breaks at 50x. Where any of these break, the plugin with `__reopen` and ack pacing after paint both bring the page back within a second.
- **What it costs the developer.** SVG keeps the morph and the plugin's reach, but the board is no longer a grid of elements: hit testing and "is this cell alive" have to come from coordinates and state, not the DOM; the board on its side, the round cells of the smooth style and per-cell CSS have to be redone in SVG terms. Canvas goes further: the drawing is script in a `data-effect`, colours and themes must be read from CSS by hand, nothing on the board is in the DOM for tests or tools, and the board travels as signals, which `newest-frame.js` never drops, so without `__reopen` the plugin no longer helps it (lag 7.9 s instead of 14.5 s at 50x, only from dropping the counters). For accessibility nothing changes here: the site's board is `aria-hidden` already and the counters say what it holds; a terminal, where the text is the content, would lose that in a canvas.

In short: on a phone up to about 8x slower than this desktop, the site's frame keeps up as it is; up to 30x and beyond, an SVG board keeps up with no fix at all, at a cost in flexibility that is moderate for SVG and larger for canvas. The fixes stay useful as a safety net: for slower devices, for frames such as a busy terminal that cannot be reduced that far, and because nothing on the server can know in advance how slow the next viewer's phone is.
