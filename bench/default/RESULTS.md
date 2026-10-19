# The default, measured again

v0.3.1 turns the hybrid in place on by default in the template and the site: `newest-frame.js` without `__reopen`, `<body>` posting `newest-frame-behind` to `/pace`, which paces the stream with a window of one, each paced write acknowledged after the paint with the page's round trip, and `Spec.Release` and `Spec.ReleaseSlack` at their defaults (5 s and 100 ms). This file checks that the site as it ships, with no switch of `TestLifeDrift` set, behaves as the hybrid rows of `bench/gaps/RESULTS.md` did.

## Setup

- Machine: tower-dev, Intel Core i5-13500 (20 threads), 62 GB, Linux 6.18.38 (Unraid), in a container, as in `bench/gaps`. Every run took `/tmp/lith-bench.lock`; the load average when each run started is the first line of its `.log` and the Load column: 0.9 to 2.0.
- Software: Go 1.27.2, headless Chromium 155.0.8059.79 (`bench/gaps` had 154), Caddy as in `bench/gaps` (TLS, HTTP/2, `flush_interval -1`), Datastar 1.0.4, `newest-frame.js` at commit fd92fa12 of datastar-attribute-newest-frame.
- Date: 2026-10-19, between 06:40 and 07:08 UTC.
- Code: `04368f6` on the branch `v0.3.1`, run from a separate worktree so that edits of the docs could not touch the build; the commit is in the first line of every `.log`.
- Harness: `TestLifeDrift` as in `bench/gaps`, with the site's default configuration: no `LIFE_DRIFT_PLUGIN`, `LIFE_DRIFT_ACK`, `LIFE_DRIFT_HYBRID`, `LIFE_DRIFT_RELEASE` or `LIFE_DRIFT_RELEASE_SLACK`. The Game of Life (10 generations a second, a fresh soup every 15 s) behind Caddy over HTTP/2, `navigator.connection` saying 4g, the delaying proxy for a far link (`LIFE_DRIFT_RTT`). Lag is how long ago the board passed the generation the page had applied last.
- `batch.sh` ran four cases, three rounds each, through `run.sh`; `table.py`, copied from `bench/gaps`, made `table-runs.md` and `table-phase.md`. Raw outputs: `NAME.log`, `NAME.csv`, `NAME.lag.csv`.

## Results

Median of three runs, range in brackets.

| Case | Lag median (s) | Lag max (s) | Applied/s | Site wrote KB/s | Acks/s | Paced at (s) | Released | Load |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 15x, fast link, 150 s | 0.35 (0.28-0.36) | 0.75 (0.68-0.75) | 2.52 (2.52-2.56) | 2.17 (2.16-2.19) | 2.49 | 2.1 (2.1-2.2) | never | 1.9 (0.9-2.0) |
| 1x, 300 ms, 90 s | 0.08 (0.07-0.09) | 0.10 (0.09-0.17) | 10.07 | 6.83 (6.74-6.85) | 0 | never | | 1.7 (1.5-2.0) |
| 15x, 300 ms, 150 s | 0.57 (0.55-0.66) | 1.15 (1.07-1.17) | 1.46 (1.43-1.47) | 1.48 (1.46-1.52) | 1.42 | 1.7 (1.6-1.7) | never | 1.6 (1.4-1.9) |
| 15x then 1x at 60 s, 300 ms: 0-60 s | 0.60 (0.59-0.63) | 1.00 (1.00-1.14) | 1.45 (1.43-1.47) | 1.95 (1.89-2.01) | 1.35 | 1.7 (1.5-1.7) | at 66 s in all three | 1.4 (1.2-1.5) |
| 60-120 s | 0.10 (0.10-0.14) | 0.43 (0.40-0.51) | 9.40 (9.38-9.42) | 6.40 (6.02-6.52) | 0.25 | | | |

The page reported round trips of 1 to 2 ms on the fast link and 301 to 305 ms on the far one, in every run. In the second run of the last case the page posted `/pace` twice, at 1.5 and 4.5 s: the plugin reported again while the page was still behind, and `Hub.Pace` left the paced stream as it was (1 paced, 1 released, as in the other runs).

## Against bench/gaps

| Case | bench/gaps row | Then | Now |
| --- | --- | --- | --- |
| 15x, fast link | Hybrid, inplace (`bench/hybrid` and A) | 0.34 s (0.30-0.37), max 0.70, 2.19 KB/s, paced at 2.2 s, never released | 0.35 s (0.28-0.36), max 0.75, 2.17 KB/s, paced at 2.1 s, never released |
| 1x, 300 ms | Hybrid, inplace (A) | 0.10 s, 10.07 frames/s, never paced, 0 acks | 0.08 s, 10.07 frames/s, never paced, 0 acks |
| 15x, 300 ms | Hybrid, inplace, slack (C) | 0.64 s (0.57-0.65), 1.43 frames/s, 1.45 KB/s, never released | 0.57 s (0.55-0.66), 1.46 frames/s, 1.48 KB/s, never released |
| | Hybrid, inplace (C) | 0.60 s (0.57-0.63), 1.45 frames/s, 1.48 KB/s | |
| | Plugin, no reopen (C) | 0.47 s (0.40-0.47), 2.74 frames/s, 6.91 KB/s | |
| 15x then 1x, 300 ms | Page-reported round trip (the release on a far link) | 0.63 s at 1.45 frames/s, then 0.13 s at 9.37; released at 65, 66 and 66 s | 0.60 s at 1.45, then 0.10 s at 9.40; released at 66 s in all three |

Every case lies within the ranges of the runs it repeats, with a newer Chromium. The shipped default paces a slow page 2 s in, never paces a fast one on a far link and sends it no acknowledgement, and releases a page that becomes fast 6 s after the speed-up on a 300 ms link. On a slow phone on that link it stays behind the plugin alone on lag, 0.57 s against 0.47 s in `bench/gaps`, for a fifth of the bytes.
