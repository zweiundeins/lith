# The gaps in the matrix

`bench/drift/RESULTS.md` and `bench/hybrid/RESULTS.md` left cells of the streams guide's matrix empty and rested some of its numbers on one run. This file fills the cells, repeats the single runs so that every row rests on three, and adds four things the earlier files did not measure: how fast the page answers input, a slow phone on a far link, a fix for the release on a far link, and a sweep of moderate slowdowns.

## Setup

- Machine: tower-dev, Intel Core i5-13500 (20 threads), 62 GB, Linux 6.18.38 (Unraid), in a container. Another agent measured frame costs in `/develop/_lith/lith-frame-cost` during the first hours; every run of both that started a browser, and the load test, took `/tmp/lith-bench.lock` first, so no two measurements overlapped. The load average when each run started is the first line of its `.log` and the Load column of the tables: 1.1 to 3.5 for every row here, most between 1.5 and 2.1.
- The container restarted twice, at about 13:04 and 13:10 UTC. Each restart killed the run in progress (`rtt100ms-ack1-1`, then the same run again) and `/tmp` with the lock file. `configs.sh` skips a run whose log has a summary, so the queue was started again and ran each cut-off run from the start. No result here comes from a cut-off run.
- Software: Go 1.27.2, headless Chromium 154.0.8037.57, Firefox 158.0, Caddy 2.10.2 (TLS, HTTP/2, `flush_interval -1`), Datastar 1.0.4, `newest-frame.js` at commit fd92fa12 of datastar-attribute-newest-frame, all as in `bench/drift` and `bench/hybrid`.
- Date: 2026-10-17, between 12:51 and 20:59 UTC.
- Code: branch `hybrid`. `27ad03b`, `a0d4d77` and `fb771d9` add what is measured here (below); `36bedff` regenerated the docs only. The far-link and slow-for-a-while runs ran at `fb771d9`, the rest at `36bedff`; the commit is in the first line of every `.log`.
- Harness: `TestLifeDrift` as in `bench/hybrid`: the site's Game of Life (10 generations a second, a fresh soup every 15 s) behind Caddy over HTTP/2, `navigator.connection` saying 4g, Chromium's CPU slowed with `Emulation.setCPUThrottlingRate`, the delaying proxy for a far link (`LIFE_DRIFT_RTT`, half the round trip each way, no bandwidth limit). Lag is how long ago the board passed the generation the page had applied last; under 0.1 s is noise.
- Configurations, as `configs.sh` names them: `off` (no fix: no plugin, no pacing), `noreopen` (the plain plugin, `data-newest-frame`), `reopen` (the plugin with `__reopen`, as the site ships), `ack1` and `ack2` (ack pacing for every stream from the start, W=1 or 2, acknowledged once applied), `ack1paint` (W=1, acknowledged after the paint), `hybrid-reopen` and `hybrid-inplace` (the hybrid of `bench/hybrid`, W=1 after the paint, `Release` 5 s), and `hybrid-slack` (inplace with the new release rule, below).
- Raw outputs in this directory: `NAME.log`, `NAME.csv`, `NAME.lag.csv`, and for the input runs `NAME.input.csv` (each key press: when, how long to the handler, how long DevTools took). `table.py` makes every table (`runs`, `phases`, `input`, `load`), saved as `table-*.md`; `batch-a.sh` to `batch-d.sh` ran everything through `batch-all.sh`.

### What the commits add

- `live`: `Spec.ReleaseSlack` and `Hub.RoundTrip(id, rtt)`. With a slack, once the page has reported its round trip, an acknowledgement counts against `Release` only if it came more than the slack after the round trip, counted from the write it acknowledges, instead of whenever a frame was waiting. The stream keeps the shortest round trip reported. Without a report it uses the old rule. Tests: `TestReleaseSlackAllowsTheRoundTrip`, `TestReleaseSlackKeepsALatePagePaced`.
- The site: `Config.AckReleaseSlack` (off unless the harness sets it). The acknowledgement's URL then carries `&rtt=`, `responseStart - requestStart` of the page's last `/ack` or `/pace` from its Resource Timing, which it clears after reading so that the buffer of 250 entries never fills; `/ack` passes it to `Hub.RoundTrip`.
- The harness: `LIFE_DRIFT_RELEASE_SLACK`, and `LIFE_DRIFT_INPUT=1`, which presses Shift every 500 ms (below). It logs the round trips the page reported.
- `TestHybridLoad`: a mode `reopen` in which the slow tenth of the pages gets the slow stream (`Spec.Slow`) and the rest every frame; `LIVE_HYBRID_LOAD_MODES` picks the modes, all four by default.

## A. The gaps, three runs each

### Fast CPU on a far link

No throttling, 90 s, three rounds with the configurations in turn (`table-rtt.md`). Median of the runs, range in brackets.

| Configuration | RTT | Lag median (s) | Lag max (s) | Applied/s | Site wrote KB/s | Acks/s | Link up / down KB/s | Site CPU | Caddy CPU | Load |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| No fix | 100 ms | 0.04 (0.03-0.05) | 0.05 (0.04-0.06) | 10.07 | 6.73 | 0 | 0.08 / 9.62 | 4.5% | 0.2% | 1.7 (1.3-3.4) |
| Plugin, no reopen | 100 ms | 0.02 (0.02-0.04) | 0.03 (0.03-0.05) | 10.07 | 6.79 | 0 | 0.08 / 9.70 | 4.1% | 0.2% | 1.9 (1.5-2.4) |
| Plugin, `__reopen` | 100 ms | 0.00 (0.00-0.02) | 0.03 (0.00-0.08) | 10.08 | 6.92 | 0 | 0.08 / 8.38 | 3.7% | 0.2% | 2.0 (1.8-2.3) |
| Ack W=1 | 100 ms | 0.06 (0.05-0.07) | 0.17 (0.16-0.18) | 7.16 (7.10-7.59) | 5.06 | 7.14 | 0.99 / 9.65 | 7.0% | 1.5% | 3.0 (1.1-3.0) |
| Ack W=2 | 100 ms | 0.05 (0.02-0.05) | 0.06 (0.03-0.06) | 10.07 | 6.83 | 10.07 | 1.36 / 12.30 | 5.5% | 1.9% | 1.9 (1.4-2.0) |
| Ack W=1 after paint | 100 ms | 0.08 (0.06-0.14) | 0.17 (0.15-0.18) | 6.61 | 4.70 | 6.61 | 0.93 / 9.16 | 7.4% | 1.6% | 1.8 (1.5-2.1) |
| Hybrid, reopen | 100 ms | 0.04 (0.00-0.04) | 0.05 (0.00-0.06) | 10.07 | 6.85 | 0 | 0.08 / 8.31 | 4.1% | 0.2% | 1.9 (1.7-2.2) |
| Hybrid, inplace | 100 ms | 0.00 (0.00-0.06) | 0.07 (0.00-0.07) | 10.07 | 6.94 | 0 | 0.08 / 8.41 | 3.7% | 0.2% | 1.7 |
| No fix | 300 ms | 0.13 (0.07-0.16) | 0.14 (0.08-0.17) | 10.07 | 6.81 | 0 | 0.08 / 9.73 | 3.7% | 0.2% | 1.7 (1.5-2.5) |
| Plugin, no reopen | 300 ms | 0.07 (0.07-0.14) | 0.17 (0.16-0.18) | 10.07 | 7.17 | 0 | 0.08 / 10.08 | 3.7% | 0.2% | 1.7 (1.6-2.2) |
| Plugin, `__reopen` | 300 ms | 0.07 (0.07-0.10) | 0.17 (0.11-0.18) | 10.07 | 6.73 | 0 | 0.08 / 8.20 | 3.8% | 0.2% | 1.7 (1.2-2.1) |
| Ack W=1 | 300 ms | 0.29 (0.25-0.31) | 0.49 (0.45-0.51) | 2.90 (2.89-2.90) | 2.32 | 2.89 | 0.44 / 5.74 | 5.8% | 0.8% | 1.6 (1.6-1.8) |
| Ack W=2 | 300 ms | 0.22 (0.16-0.28) | 0.36 (0.33-0.40) | 5.84 (5.84-5.92) | 4.29 | 5.84 | 0.83 / 8.55 | 6.7% | 1.5% | 1.6 (1.4-1.9) |
| Ack W=1 after paint | 300 ms | 0.29 (0.27-0.34) | 0.47 (0.45-0.49) | 2.83 | 2.31 | 2.82 | 0.43 / 5.71 | 5.9% | 0.7% | 1.3 (1.1-1.8) |
| Hybrid, reopen | 300 ms | 0.07 (0.07-0.10) | 0.11 (0.08-0.17) | 10.07 | 6.82 | 0 | 0.08 / 8.28 | 3.6% | 0.2% | 2.1 (1.6-3.5) |
| Hybrid, inplace | 300 ms | 0.10 (0.07-0.11) | 0.12 (0.11-0.18) | 10.07 | 6.70 | 0 | 0.08 / 8.17 | 3.8% | 0.2% | 1.7 (1.4-2.2) |

The plain plugin, the cell that was missing, costs nothing on a far link: it never dropped a frame, applied 10.07 a second at both round trips and asked for nothing. The three runs reproduce every single run of `bench/drift` and `bench/hybrid` within a few hundredths of a frame a second: ack pacing with W=1 caps the page at about one frame per round trip (7.16 and 2.90 a second, 6.61 and 2.83 when it acknowledges after the paint), W=2 at two (10.07 and 5.84), and neither hybrid ever switched.

### Slow for 60 s, then fast

15x CPU for 60 s, then 1x, 120 s (no fix 180 s), three rounds (`table-phase.md`). "Drained after" is how long after the speed-up the lag first fell under 0.2 s.

| Configuration | Phase (s) | Lag median (s) | Lag max (s) | Applied/s | Painted/s | Site wrote KB/s | Acks/s | Events | Load |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| No fix | 0-60 (15x) | 11.76 (11.50-12.33) | 20.88 (20.52-20.97) | 7.02 | 0.17 | 7.54 | 0 | 20.88 s behind at 60 s; drained after 7 s (6-7) | 1.6 (1.5-1.8) |
| | 60-120 (1x) | 0.00 | 21.68 (21.22-21.98) | 13.57 | 9.13 | 6.94 | 0 | | |
| | 120-180 (1x) | 0.00 | 0.00 | 10.07 | 10.07 | 6.78 | 0 | | |
| Plugin, no reopen | 0-60 | 0.30 (0.27-0.35) | 0.87 (0.80-0.96) | 2.73 | 2.72 | 7.07 | 0 | never asks | 1.8 (1.7-1.9) |
| | 60-120 | 0.00 | 0.00 | 10.03 | 10.03 | 7.56 | 0 | | |
| Plugin, `__reopen` | 0-60 | 0.67 (0.55-0.69) | 1.18 (0.94-1.20) | 2.82 | 2.80 | 3.71 | 0 | `Save-Data` stream at 2.1 s | 2.1 (1.9-2.2) |
| | 60-120 | 0.30 (0.28-0.35) | 0.40 (0.38-0.45) | 4.02 | 4.02 | 3.02 | 0 | stays on the slow stream | |
| Ack W=1 after paint, every stream | 0-60 | 0.29 (0.26-0.30) | 0.65 (0.65-0.68) | 2.57 | 2.55 | 1.94 | 2.55 | paced from the start | 2.1 (1.5-2.2) |
| | 60-120 | 0.00 | 0.00 | 10.05 | 10.05 | 6.68 | 10.07 | | |
| Hybrid, reopen | 0-60 | 0.31 (0.28-0.35) | 0.75 (0.71-0.87) | 2.50 | 2.48 | 2.64 | 2.42 | `Save-Data` stream at 2.1 s; released at 66 s in all three | 2.1 (1.6-2.3) |
| | 60-120 | 0.00 | 0.00 (0.00-0.01) | 10.03 | 10.05 | 6.75 | 0.88 | | |
| Hybrid, inplace | 0-60 | 0.30 (0.27-0.35) | 0.76 (0.69-0.76) | 2.57 | 2.55 | 2.58 | 2.48 | `/pace` at 2.1 s; released at 66 s in all three | 1.8 (1.4-2.1) |
| | 60-120 | 0.00 | 0.00 | 10.05 | 10.05 | 7.14 | 0.88 | | |

The page without a fix does drain its backlog, and fast: it was 20.5 to 21.0 s behind when the CPU sped up, finished the task it was in (the lag still grew for 3 to 4 s, to 21.2 to 22.0 s), then applied about 100 frames a second for two seconds and was current 6 to 7 s after the speed-up, in all three runs. A backlog of 20 s at 15x is about 200 frames, which at full speed cost about 10 ms each. The 180 s runs show nothing after that: 10.07 frames a second from 120 s on.

The plain plugin and ack pacing for every stream both return to 10 frames a second at once, since neither changes the stream; ack pacing then costs 10 acknowledgements a second for good. The hybrids release 6 s after the speed-up in every run, as in `bench/hybrid`'s single run; the plugin with `__reopen` stays on the slow stream at 4.02 frames a second, 0.30 s behind, in every run.

### Firefox

Each board frame costs the page 150 ms more (`LIFE_DRIFT_COST`), 150 s, three rounds (`table-firefox.md`).

| Configuration | Lag median (s) | Lag max (s) | Applied/s | Painted/s | Site wrote KB/s | Acks/s | Switch at (s) | Load |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Hybrid, inplace | 0.29 (0.28-0.32) | 0.41 (0.39-0.44) | 4.13 | 4.13 | 3.28 | 4.05 | 2.7 | 1.8 (1.6-4.8) |
| Plugin, `__reopen` | 0.49 (0.48-0.53) | 0.67 (0.65-0.70) | 4.00 | 3.99 | 3.18 | 0 | 2.6 | 1.6 (1.6-2.0) |
| No fix | 32.04 (31.59-32.09) | 63.09 (62.61-63.15) | 5.86 | 5.52 | 6.69 | 0 | never | 1.8 (1.8-1.9) |

The single runs hold: the hybrid stays 0.29 s behind against 0.49 s on the slow stream. The new row, no fix, falls 63 s behind in 150 s, as `bench/drift` found for Firefox over 120 s (51.5 s then). Unlike Chromium, Firefox keeps painting (5.52 a second, against 0.16 in Chromium) because it hands the page one frame per task, so it shows every frame it applies, but each is a minute old by the end.

### Many viewers, with the slow stream

`TestHybridLoad`, as in `bench/hybrid` (one view of about 48 KB of HTML, 10 frames a second, zstd, a tenth of the pages slow), the four modes in turn, three rounds, 5 s warm-up and 20 s measured (`load-direct.log`, `load-http.log`, `table-load.md`, loads 2.08 and 1.51). In `reopen` the slow pages are on the slow stream from the start; `__reopen` moves them after about 2 s, within the warm-up.

| Streams | Mode | CPU, acks direct | CPU, acks over HTTP | Acks/s | Wire KB/s | Frames sent/s |
| --- | --- | --- | --- | --- | --- | --- |
| 200 | Plugin (and no fix) | 48% (48-50) | 49% (48-50) | 0 | 3,375 | 1,990 |
| 200 | Plugin, `__reopen` | 48% (46-48) | 47% (46-48) | 0 | 3,175 | 1,871 |
| 200 | Ack, every stream | 50% (49-51) | 97% (94-98) | 1,841 | 3,117 | 1,841 |
| 200 | Hybrid | 46% (46-48) | 49% (49-51) | 50 | 3,124 | 1,841 |
| 1,000 | Plugin (and no fix) | 149% (144-175) | 150% (142-155) | 0 | 16,876 | 9,950 |
| 1,000 | Plugin, `__reopen` | 161% (146-162) | 155% (144-161) | 0 | 15,849 | 9,351 |
| 1,000 | Ack, every stream | 154% (153-183) | 439% (422-449) | 9,205 | 15,586 | 9,205 |
| 1,000 | Hybrid | 140% (138-178) | 156% (152-193) | 250 | 15,617 | 9,205 |

The server does the same for a page without a fix as for one with the plain plugin: every stream gets every frame, and the plugin acts only in the page. So the "plugin" mode stands for both. With `__reopen` the slow hundred get four frames a second instead of ten: 6% fewer bytes than sending every frame, no requests, and a CPU within the plugin's range. The hybrid sends 1.5% fewer bytes than that, since its slow pages acknowledge only 2.5 frames a second, for 250 requests a second. Pacing every stream costs 439% of a core over HTTP against 150 to 156% for the others, as in `bench/hybrid` (452% then).

## B. Input responsiveness

15x CPU, fast link, 150 s, three rounds. `LIFE_DRIFT_INPUT=1` sends Shift down and up through CDP's `Input.dispatchKeyEvent` every 500 ms, each press on its own goroutine so that a page holding one up does not delay the next, as `TestSlowLinkCost` did for the streams guide's "slowest key" (that test timed DevTools' reply; this one also notes when the page's own `keydown` listener runs, a capture listener on `window` that logs the time to the console). An observer of Chromium's Event Timing (`type: 'event'`, `durationThreshold: 16`) reports each `keydown` and `keyup` entry; the INP-like p98 counts presses without an entry as under 16 ms. "To the handler" is from dispatch to the listener, by the page's clock and the test's, which agree within a few milliseconds here (the DevTools column comes out 0 to 4 ms longer). Percentiles over all presses of the three runs (`table-input.md`).

| Configuration | Presses | To the handler: median / p95 / max (ms) | DevTools' reply: median / p95 / max (ms) | Event Timing keydown p98 (range) / max (ms) | Lag median (s) | Applied/s | Load |
| --- | --- | --- | --- | --- | --- | --- | --- |
| No fix | 896 | 2,970 / 5,949 / 6,581 | 2,971 / 5,949 / 6,582 | 6,368 (6,304-6,440) / 6,688 | 25.48 | 7.08 | 1.4 (1.4-1.7) |
| Plugin, no reopen | 900 | 142 / 331 / 529 | 143 / 333 / 530 | 480 (464-480) / 656 | 0.28 | 2.55 | 1.4 (1.4-1.8) |
| Plugin, `__reopen` | 900 | 132 / 293 / 463 | 135 / 294 / 464 | 448 (408-464) / 592 | 0.63 | 2.70 | 1.7 (1.3-1.8) |
| Ack W=1 after paint, every stream | 900 | 118 / 301 / 443 | 120 / 302 / 445 | 448 / 576 | 0.29 | 2.49 | 1.7 (1.6-1.7) |
| Hybrid, inplace | 900 | 118 / 312 / 449 | 122 / 312 / 449 | 464 / 584 | 0.34 | 2.39 | 1.7 (1.5-1.8) |

Without a fix a key press waits for the task of 45 frames to end: 3 s at the median, up to 6.6 s, and Event Timing's p98 is 6.4 s. All four fixes bring it to 118 to 142 ms at the median and under 530 ms at most, and they cannot be told apart by much: the plugin without reopen is the slowest by about 20 ms at the median and p95, since its page still receives and parses all ten frames a second. None reaches "good" by INP's measure: every press had an Event Timing entry of 16 ms or more, and the p98 of 448 to 480 ms is in INP's "needs improvement" band (200 to 500 ms), because at 15x a frame's rendering takes most of the main thread whatever the stream sends (`bench/drift`). In `TestSlowLinkCost` at 4x the slowest key was 52 to 71 ms; at 15x it is 443 to 529 ms with a fix and 6.6 s without.

## C. A slow phone on a far link

15x CPU and 100 or 300 ms round trip, 150 s, three rounds (`table-c.md`). `hybrid-slack` is the inplace hybrid with the release measured against the round trip the page reports (`ReleaseSlack` 100 ms, below).

| Configuration | RTT | Lag median (s) | Lag max (s) | Applied/s | Painted/s | Site wrote KB/s | Acks/s | Link up / down KB/s | Switch at (s) | Released | Load |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| No fix | 100 ms | 25.61 (25.44-27.11) | 47.11 (46.53-48.10) | 7.17 | 0.16 | 7.27 | 0 | 0.08 / 9.17 | never | | 1.7 (1.7-2.0) |
| Plugin, `__reopen` | 100 ms | 0.70 (0.69-0.73) | 1.19 (1.10-1.23) | 2.87 | 2.86 | 3.26 | 0 | 0.05 / 4.08 | 2.1 | | 1.5 (1.5-1.6) |
| Plugin, no reopen | 100 ms | 0.35 (0.34-0.39) | 0.83 (0.78-0.85) | 2.69 | 2.68 | 7.01 | 0 | 0.05 / 8.88 | never | | 1.6 (1.4-1.8) |
| Ack W=1 after paint | 100 ms | 0.39 (0.34-0.41) | 0.83 (0.79-0.83) | 2.07 | 2.07 | 1.69 | 2.07 | 0.32 / 3.85 | from the start | | 1.7 (1.6-2.0) |
| Ack W=2 | 100 ms | 0.50 (0.49-0.51) | 1.20 (1.19-1.21) | 3.72 | 1.91 | 2.77 | 3.71 | 0.52 / 5.40 | from the start | | 1.5 (1.5-1.6) |
| Hybrid, inplace | 100 ms | 0.38 (0.36-0.41) | 0.80 (0.76-0.87) | 2.08 | 2.07 | 1.89 | 2.04 | 0.31 / 3.20 | 2.2 | never | 1.5 (1.5-1.7) |
| Hybrid, inplace, slack | 100 ms | 0.41 (0.36-0.42) | 0.91 (0.86-0.93) | 2.04 | 2.03 | 1.92 | 2.01 | 0.32 / 3.22 | 2.1 (1.4-2.3) | never | 1.6 (1.5-1.9) |
| No fix | 300 ms | 26.11 (25.40-27.23) | 47.11 (47.01-48.74) | 7.13 | 0.16 | 7.09 | 0 | 0.05 / 8.96 | never | | 1.8 (1.7-2.2) |
| Plugin, `__reopen` | 300 ms | 0.80 (0.76-0.81) | 1.25 (1.20-1.30) | 2.84 | 2.83 | 3.30 | 0 | 0.05 / 4.12 | 1.7 (1.5-1.7) | | 1.6 (1.5-1.9) |
| Plugin, no reopen | 300 ms | 0.47 (0.40-0.47) | 0.88 (0.87-1.50) | 2.74 | 2.72 | 6.91 | 0 | 0.05 / 8.78 | never | | 1.7 (1.6-1.8) |
| Ack W=1 after paint | 300 ms | 0.55 (0.55-0.57) | 1.05 (1.04-1.07) | 1.45 | 1.44 | 1.21 | 1.44 | 0.23 / 3.20 | from the start | | 1.6 (1.6-2.0) |
| Ack W=2 | 300 ms | 0.62 (0.55-0.63) | 1.23 (1.15-1.24) | 2.94 | 2.72 | 2.26 | 2.92 | 0.42 / 4.68 | from the start | | 1.7 (1.4-1.7) |
| Hybrid, inplace | 300 ms | 0.60 (0.57-0.63) | 1.14 (0.97-1.29) | 1.45 | 1.43 | 1.48 | 1.40 | 0.23 / 2.60 | 1.7 (1.5-2.0) | never | 1.6 (1.5-1.7) |
| Hybrid, inplace, slack | 300 ms | 0.64 (0.57-0.65) | 1.14 (1.05-1.35) | 1.43 | 1.42 | 1.45 | 1.40 | 0.28 / 2.60 | 1.7 (1.5-1.7) | never | 1.8 |

What changes against a fast link (15x, `bench/hybrid`):

- The round trip adds to the lag of every fix, and most to the paced ones. At W=1 a paced page gets one frame per round trip plus the time to apply and paint it: 2.07 frames a second at 100 ms and 1.45 at 300 ms, against 2.5 on a fast link. At 300 ms the hybrid stays 0.60 s behind, ack pacing 0.55 s, against 0.47 s for the plain plugin, whose page drops what it cannot apply but never waits for a round trip.
- The plain plugin has the lowest lag on a far link at 15x (0.35 and 0.47 s), and the most bytes (6.9 to 7.0 KB/s). The paced configurations send the fewest (1.2 to 1.9 KB/s). The slow stream of `__reopen` sits between on bytes (3.3 KB/s) and is last on lag (0.70 and 0.80 s).
- W=2 hides half the round trip: 3.72 frames applied a second at 100 ms and 2.94 at 300 ms, but at 100 ms the two frames of a window often arrive together and the page paints once for both (1.91 painted a second), so it does not show more than W=1 and lags more (0.50 s).
- The switch comes earlier at 300 ms (1.7 s against 2.1 to 2.2 s): the plugin judges by frames dropped, and the frames that piled up during the first round trips arrive together.
- No configuration released, and none should: the page stayed slow. The slack variant did not release either, in any of the six runs: it does not mistake a slow page on a far link for a fast one.

### The release on a far link

`bench/hybrid` expected that a paced page could never be released over a round trip longer than one tick. To see it, the page must become fast while paced: 15x for 60 s, then 1x, 120 s, at 100 and 300 ms, three rounds each, with the old rule (`hybrid-inplace`) and the new one (`hybrid-slack`) (`table-release.md`).

| Configuration | RTT | Phase (s) | Lag median (s) | Lag max (s) | Applied/s | Site wrote KB/s | Acks/s | Released | Load |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Old rule | 100 ms | 0-60 (15x) | 0.42 (0.38-0.47) | 0.84 (0.73-0.99) | 2.10 | 2.25 | 2.00 | never, in all three | 2.0 (1.3-2.1) |
| | | 60-120 (1x) | 0.12 (0.08-0.12) | 0.14 (0.14-0.19) | 6.62 | 5.04 | 6.63 | | |
| Page-reported round trip | 100 ms | 0-60 | 0.42 (0.40-0.44) | 0.82 (0.80-0.84) | 2.07 | 2.23 | 1.98 | at 66 s, in all three | 1.5 (1.5-1.9) |
| | | 60-120 | 0.03 (0.00-0.04) | 0.14 (0.13-0.20) | 9.72 | 6.43 | 0.55 | | |
| Old rule | 300 ms | 0-60 | 0.58 (0.55-0.59) | 1.15 (0.99-1.18) | 1.50 | 1.85 | 1.40 | never, in all three | 1.6 (1.5-1.7) |
| | | 60-120 | 0.29 (0.29-0.34) | 0.50 (0.45-0.50) | 2.82 | 2.24 | 2.82 | | |
| Page-reported round trip | 300 ms | 0-60 | 0.63 (0.59-0.65) | 1.19 (1.03-1.35) | 1.45 | 1.96 | 1.35 | at 65, 66 and 66 s | 1.7 (1.6-1.8) |
| | | 60-120 | 0.13 (0.09-0.15) | 0.46 (0.43-0.49) | 9.37 | 6.58 | 0.25 | | |
| Page-reported round trip, fast link | | 0-60 | 0.33 (0.29-0.36) | 0.69 (0.67-0.84) | 2.48 | 2.60 | 2.38 | at 66 s, in all three | 1.6 (1.6-1.7) |
| | | 60-120 | 0.00 | 0.00 | 10.05 | 7.08 | 0.88 | | |

The limit is real, and it starts at one tick, not above it: with the old rule a page that became fast at 60 s stayed paced to the end at 100 ms as at 300 ms, at 6.62 and 2.82 frames a second, the rates of ack pacing from the start (A). Every acknowledgement takes at least a round trip, by which time the hub has rendered the next frame, so the hub always found one waiting.

The fix, first try (`slack-fastest-try.log`, one run at `a0d4d77`, 15x for 20 s then 1x, 300 ms): take the fastest acknowledgement the stream has seen as its round trip, and count an acknowledgement as late when it comes more than a tick after that. It released the page 5 s after it was paced, while it was still at 15x, and the plugin asked for pacing again 2 s later. At one frame per round trip a slow page has time to spare between frames, so its acknowledgements come at a steady delay: the fastest of them includes the page's own time, and the rule then measures only the jitter.

The fix as committed (`fb771d9`): the page reports its round trip. Its Resource Timing has, for its last `/ack`, the time from sending the request to the first byte of the 204, which the browser's network stack takes whatever the main thread is doing; the page appends it to its next acknowledgement, and the stream keeps the shortest. An acknowledgement is late when it comes more than `ReleaseSlack` (100 ms, one tick) after that round trip, counted from the write. The reported round trips were 101 to 105 ms at 100 ms and 301 to 305 ms at 300 ms in every run, and 1 ms on the fast link.

With it, all six far-link runs released 5 to 6 s after the speed-up, as on a fast link, and the page then applied 9.72 and 9.37 frames a second instead of 6.62 and 2.82. On the fast link it released at 66 s as the old rule does. It did not release a page that stayed slow in any of the nine runs at 15x (three each at 100 ms, at 300 ms and on the fast link, `slow-hybrid-slack`: 0.30 s behind, 2.55 frames a second, as `hybrid-inplace` in `bench/hybrid`). It costs the page one Resource Timing lookup per acknowledgement and the server a comparison. It is off unless the app sets `ReleaseSlack`, and needs the page to report the round trip; the site's wiring does that only when `AckReleaseSlack` is set.

## D. Moderate slowdowns

Fast link, 90 s, one run at 4x, 10x and 15x, three at 6x and 8x, where the page without a fix still applies every frame but no longer paints each (`table-d.md`; `batch-d.sh`, `batch-d2.sh`). "Painted" counts the rendering updates after an applied frame. Loads 1.3 to 2.6.

| CPU | Configuration | Runs | Lag median (s) | Lag max (s) | Applied/s | Painted/s | Site wrote KB/s | Acks/s | Switched |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 4x | No fix | 1 | 0.00 | 0.20 | 10.12 | 9.79 | 7.02 | 0 | |
| | Plugin, no reopen | 1 | 0.01 | 0.11 | 9.88 | 9.87 | 6.91 | 0 | |
| | Plugin, `__reopen` | 1 | 0.01 | 0.11 | 9.86 | 9.84 | 7.16 | 0 | never |
| | Ack W=1 after paint | 1 | 0.01 | 0.11 | 9.69 | 9.68 | 6.82 | 9.69 | paced from the start |
| | Hybrid, inplace | 1 | 0.01 | 0.03 | 9.90 | 9.89 | 6.76 | 0 | never |
| 6x | No fix | 3 | 0.10 (0.05-0.13) | 0.43 (0.34-0.51) | 10.14 | 6.49 (6.17-6.78) | 7.02 | 0 | |
| | Plugin, no reopen | 3 | 0.06 (0.04-0.07) | 0.26 (0.14-0.36) | 7.89 | 7.88 | 7.00 | 0 | |
| | Plugin, `__reopen` | 3 | 0.37 (0.36-0.38) | 0.48 (0.46-0.50) | 4.03 | 4.02 | 3.33 | 0 | at 2.4 s (2.4-2.5) |
| | Ack W=1 after paint | 3 | 0.10 (0.06-0.12) | 0.24 (0.21-0.26) | 6.77 | 6.76 | 4.61 | 6.77 | paced from the start |
| | Hybrid, inplace | 3 | 0.09 (0.07-0.11) | 0.19 (0.17-0.23) | 6.71 | 6.70 | 4.75 | 6.54 | at 2.5 s, never released |
| 8x | No fix | 3 | 0.37 (0.34-0.40) | 1.00 (0.84-1.07) | 10.14 | 2.56 (2.42-2.63) | 6.77 | 0 | |
| | Plugin, no reopen | 3 | 0.12 (0.11-0.12) | 0.23 | 5.70 | 5.69 | 6.96 | 0 | |
| | Plugin, `__reopen` | 3 | 0.39 (0.37-0.42) | 0.58 (0.51-0.63) | 3.99 | 3.98 | 3.38 | 0 | at 2.4 s (2.4-2.5) |
| | Ack W=1 after paint | 3 | 0.16 (0.10-0.19) | 0.40 (0.30-0.45) | 4.87 | 4.86 | 3.52 | 4.87 | paced from the start |
| | Hybrid, inplace | 3 | 0.13 (0.10-0.13) | 0.30 (0.24-0.44) | 4.79 | 4.78 | 3.71 | 4.68 | at 2.4 s, never released |
| 10x | No fix | 1 | 1.58 | 2.20 | 10.13 | 0.73 | 6.77 | 0 | |
| | Plugin, no reopen | 1 | 0.16 | 0.56 | 4.41 | 4.40 | 7.19 | 0 | |
| | Plugin, `__reopen` | 1 | 0.39 | 0.69 | 3.87 | 3.86 | 3.25 | 0 | at 2.2 s |
| | Ack W=1 after paint | 1 | 0.16 | 0.45 | 4.01 | 4.01 | 2.93 | 4.00 | paced from the start |
| | Hybrid, inplace | 1 | 0.18 | 0.48 | 3.90 | 3.89 | 3.22 | 3.81 | at 2.3 s, never released |
| 15x | No fix | 1 | 15.91 | 28.21 | 7.13 | 0.17 | 6.79 | 0 | |
| | Plugin, no reopen | 1 | 0.30 | 0.89 | 2.64 | 2.63 | 7.31 | 0 | |
| | Plugin, `__reopen` | 1 | 0.63 | 1.13 | 2.91 | 2.90 | 3.30 | 0 | at 2.2 s |
| | Ack W=1 after paint | 1 | 0.32 | 0.71 | 2.61 | 2.60 | 1.95 | 2.61 | paced from the start |
| | Hybrid, inplace | 1 | 0.33 | 0.75 | 2.51 | 2.50 | 2.39 | 2.46 | at 2.1 s, never released |

(The 15x row ran 90 s, so its "no fix" lag is lower than the 150 s runs of A.)

Where each starts to act:

- **No fix** never acts, and it degrades in two steps. From 6x the page still applies all ten frames a second but paints only some (6.49 a second at 6x, 2.56 at 8x, 0.73 at 10x): Chromium skips the renders it has no time for, so the page stays within 0.1 to 0.4 s of the board by what it applied while it shows fewer frames. From 10x it no longer applies them all in time, and from there the backlog grows: 1.58 s at the median at 10x after 90 s, and 50 s behind in 150 s at 15x.
- **The plain plugin** acts from 6x without a switch: it drops what arrives while the page is busy, and the page applies and paints 7.89 frames a second at 6x and 5.70 at 8x, a frame per paint.
- **`__reopen` and the hybrid** switch from 6x on, 2.1 to 2.5 s into every run, and never at 4x. The hybrid was never released in a run that stayed slow, as the rule says: a page that applies 4 to 7 frames a second sits in the gap between the two rules.
- **Ack pacing for every stream** acts from the start everywhere, and at 4x costs a little: 9.69 frames a second against 9.88 to 10.12, and 9.69 acknowledgements a second for a page that needs none.

Whether a fix makes things worse than no fix in the borderline zone:

- **`__reopen` does, at 6x and 8x.** It moves the page to the slow stream of 4 frames a second, 0.37 and 0.39 s behind at the median, where without a fix the page was 0.10 and 0.37 s behind and painted 6.49 and 2.56 frames a second. At 6x the page with `__reopen` shows fewer frames (4.02 a second against 6.49) and lags more, for half the bytes. At 8x it lags the same as no fix and shows more frames (3.98 against 2.56). The slow stream's cap of four frames is the cost: it is right for 15x, too low for a page that could manage six or seven.
- **Pacing does not.** At 6x the hybrid and ack pacing lag 0.09 and 0.10 s, as no fix (0.10 s), and the page paints every frame it gets (6.70 and 6.76 a second, against 6.49), for two thirds of the bytes. At 8x they lag 0.13 and 0.16 s against 0.37 s without a fix.
- **The plain plugin** is the best of all in the zone by lag (0.06 s at 6x, 0.12 s at 8x) and frames shown (7.88 and 5.69 a second), at full bytes.

## Conclusion

What changed compared to `bench/drift` and `bench/hybrid`:

- **The release on a far link: confirmed, and fixed.** The limit `bench/hybrid` expected shows from a round trip of one tick (100 ms), not only above it: with `Release` alone a paced page that became fast stayed paced at 6.62 frames a second at 100 ms and 2.82 at 300 ms, in all six runs. `Spec.ReleaseSlack` with the round trip the page reports released it 5 to 6 s after the speed-up in all six, after which it applied 9.72 and 9.37 frames a second, and it released no page that stayed slow in nine runs. The obvious server-only fix, the fastest acknowledgement as the round trip, released a page still at 15x. The page-reported round trip needs one Resource Timing lookup per acknowledgement in the page; it stays off unless an app sets it.
- **A slow phone on a far link favours the plain plugin on lag.** At 15x and 300 ms the plain plugin is 0.47 s behind against 0.55 to 0.64 s for every paced configuration and 0.80 s for `__reopen`: at W=1 a paced page waits a round trip for each frame and gets 1.45 a second. The paced configurations still send the fewest bytes (1.2 to 1.5 KB/s against 6.9). `bench/hybrid` called the hybrid as good as ack pacing and better than `__reopen` on lag; that holds on a fast link, not on a far one, where it trails the plain plugin.
- **No fix recovers on its own.** A page 21 s behind after 60 s at 15x is current again 6 to 7 s after the CPU speeds up: the backlog sits in the browser and costs little to apply at full speed. Only `__reopen` does not recover: the page stays on the slow stream, in every run.
- **Input.** At 15x every fix brings a key press from 3 s at the median (6.6 s at most) to 118 to 142 ms, and they differ little, the plain plugin slowest by about 20 ms. None reaches "good" by INP: the p98 of 448 to 480 ms is still "needs improvement", because rendering each frame keeps the main thread busy.
- **The borderline zone.** At 6x and 8x a page without a fix applies every frame but shows fewer, and stays within 0.4 s. `__reopen` makes it worse at 6x (0.37 s behind at 4 frames a second, against 0.10 s and 6.49 painted), and is no better at 8x. Pacing and the plain plugin do not make it worse. `__reopen`'s four frames a second fit a page at 15x, not one at 6x.
- **Many viewers, with `__reopen`.** At 1,000 streams with a tenth slow, `__reopen` costs what the plugin costs (155% of a core over HTTP, 150% for every frame to everyone), sends 6% fewer bytes and no requests; the hybrid sends 1.5% fewer bytes than that for 250 requests a second and 156%. Pacing every stream costs 439%. The page without a fix costs the server exactly what the plugin's does.
- **Three runs where there was one** reproduce every single run of `bench/drift` and `bench/hybrid` (far link, slow then fast, Firefox) within their ranges. Nothing earlier rested on a lucky run.

What it means for the choice: the inplace hybrid is still the best default for a public page on a fast link, and with `ReleaseSlack` it no longer traps a page on a far link. Where viewers are known to be on far links and slow phones, the plain plugin gives the lowest lag at full bytes; and `__reopen`'s fixed four frames a second costs a page that is only somewhat slow.
