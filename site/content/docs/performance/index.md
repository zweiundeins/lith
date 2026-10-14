---
title: Performance
headline: lith performance: the numbers to plan an app with
summary: The numbers to plan a lith app with, measured in process on a development machine and on the 10-dollar VPS that serves this site, the settings they led to, and the commands that measure them again.
description: Writer throughput, stream memory and compression, measured in process on a development machine and on the 10-dollar VPS that serves this site.
order: 5
---

lith's numbers come from benchmarks in its repository, run in process, and recorded in [`bench/RESULTS.md`](https://github.com/zweiundeins/lith/blob/main/bench/RESULTS.md) and the CSV files of [`bench/budget`](https://github.com/zweiundeins/lith/tree/main/bench/budget) with their date, their settings and the command that produced them. These pages show them, with charts drawn from those numbers.

## The numbers to plan with

On a development machine (below), unless a row says otherwise:

| What | Measured | Page |
| --- | --- | --- |
| Commands a second, 200 producers waiting with `Do` | 66,688 with `synchronous=NORMAL`, 34,893 with `FULL` | [The writer](/docs/performance/writer) |
| One producer alone | 13,049 a second with NORMAL; 526 with FULL, one fsync each | [The writer](/docs/performance/writer) |
| A command's commit, beside 200 streams | 0.29 to 0.51 ms at p50 with NORMAL, 2.7 to 5.5 ms with FULL | [The shared board](/docs/performance/render-loop) |
| 200 streams on one 139 KB view, 800 commands a second | one render a tick, no dropped frames, 1.82 cores with zstd, 3.42 with brotli | [The shared board](/docs/performance/render-loop) |
| Bytes per frame of that board, 142,067 bytes of HTML | 2,213 with zstd (64 to 1), 854 with brotli (166 to 1) | [Encoders](/docs/performance/encoders) |
| Heap per open stream | 1.65 MiB with zstd at 2^19, 1.0 MiB with its low-memory history, 1.55 MiB with brotli at quality 4 | [Encoders](/docs/performance/encoders) |
| The window a 142 KB frame needs | 2^18: at 2^17 a frame takes four times the bytes with zstd, a hundred times with brotli | [The stream budget](/docs/performance/budget) |
| An idle tab, hibernating after 30 s | 0.11 MiB of heap instead of 0.64, for about 45 KB more on the wire an hour | [Hibernation](/docs/performance/hibernation) |

## On this site's server

lith.zweiundeins.gmbh runs on a VPS for about 10 dollars a month: four vCPUs of an AMD EPYC (Genoa) and 4 GB of memory, shared with other apps and with the other tenants of its host. The same benchmarks ran there on 2026-10-14, medians of three runs:

| What | On the VPS |
| --- | --- |
| Commands a second, 200 producers waiting with `Do` | 80,530 with NORMAL, 14,480 with FULL |
| One producer alone | 13,436 a second with NORMAL; 143 with FULL, where an fsync takes about 7 ms |
| A command's commit, beside 200 streams, NORMAL | 0.42 ms at p50 |
| 200 streams on one 139 KB view, 800 commands a second, zstd | one render a tick, 0.55 of a core, 2,188 bytes a frame of 142,067 bytes of HTML (65 to 1) |

The writer does more with NORMAL on the VPS than on the development machine, and less with FULL, where it waits for a slower disk. In one of the three board runs the neighbours held a tick for 252 ms, and the streams skipped to the newest frame. `bench/RESULTS.md` has every row and the commands.

kanban runs on lith. On the development machine on 2026-10-14, kanban on lith v0.2.0 committed 15,341 moves a second at 200 people in its writer benchmark; in its load test with 200 tabs, its server answered a move in 12.1 ms at p95 with brotli, and needed half the CPU with zstd that it needed with brotli.

## The development machine

Unless a page says otherwise: an Intel i5-13500 (20 threads) under Linux 6.18, Go 1.26.5 to 1.27.2 and modernc.org/sqlite v1.60.1 (SQLite 3.53.4), measured from 2026-10-12 to 2026-10-14. The database sits in `/tmp`, on a btrfs image on the host's SSD cache pool. The runs are pinned to the CPU's twelve performance cores with `taskset -c 0-11`, which gives GOMAXPROCS 12.

The host runs other services, and other work ran during most measurements: `RESULTS.md` records the load average around each set. Tables show medians of three to six runs, with the range in brackets where the runs differ. Compare runs made under the same conditions, and measure an app's own views before quoting a number for it: the [load testing guide](/docs/guides/load-testing) shows how.

## Measuring again

Each page ends with the commands that produced its numbers. The writer's and the board's benchmarks run with `go test -bench`; the budget's tests run only when `LITH_BUDGET=1` is set, and write the CSV files the charts read. When a CSV file changes, `go generate ./content` in `site/` draws the charts again, and the site's tests fail until it has.
