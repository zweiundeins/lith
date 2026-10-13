---
title: Performance
summary: The numbers to plan a lith app with, measured on one machine in process, the settings they led to, and the commands that measure them again.
order: 5
---

lith's numbers come from benchmarks in its repository, run in process on one machine, and recorded in [`bench/RESULTS.md`](https://github.com/zweiundeins/lith/blob/main/bench/RESULTS.md) and the CSV files of [`bench/budget`](https://github.com/zweiundeins/lith/tree/main/bench/budget) with their date, their settings and the command that produced them. These pages show them, with charts drawn from those numbers.

## The numbers to plan with

| What | Measured | Page |
| --- | --- | --- |
| Commands a second, 200 producers waiting with `Do` | 66,688 with `synchronous=NORMAL`, 34,893 with `FULL` | [The writer](/docs/performance/writer) |
| One producer alone | 13,049 a second with NORMAL; 526 with FULL, one fsync each | [The writer](/docs/performance/writer) |
| A command's commit, beside 200 streams | 0.30 to 0.54 ms at p50 with NORMAL, 3.4 to 11.0 ms with FULL | [The shared board](/docs/performance/render-loop) |
| 200 streams on one 139 KB view, 800 commands a second | one render a tick, no dropped frames, 1.59 cores with zstd, 3.31 with brotli | [The shared board](/docs/performance/render-loop) |
| Bytes per frame of that board | 2,205 with zstd, 955 with brotli | [Encoders](/docs/performance/encoders) |
| Heap per open stream | 1.65 MiB with zstd at 2^19, 1.0 MiB with its low-memory history, 2.0 MiB with brotli at quality 4 | [Encoders](/docs/performance/encoders) |
| The window a 142 KB frame needs | 2^18: at 2^17 a frame takes four times the bytes | [The stream budget](/docs/performance/budget) |
| An idle tab, hibernating after 30 s | 0.11 MiB of heap instead of 0.64, for about 45 KB more on the wire an hour | [Hibernation](/docs/performance/hibernation) |

kanban, on lith, commits 14,928 moves a second at 200 people in its own load test and answers a move in 13.1 ms at p95; with zstd its server needs half the CPU it needed with brotli.

## The machine

Unless a page says otherwise: an Intel i5-13500 (20 threads) under Linux 6.18, Go 1.26.5 to 1.27.2 and modernc.org/sqlite v1.60.1 (SQLite 3.53.4), measured on 2026-10-12 and 2026-10-13. The database sits in `/tmp`, on a btrfs image on the host's SSD cache pool. The runs are pinned to the CPU's twelve performance cores with `taskset -c 0-11`, which gives GOMAXPROCS 12.

The host runs other services, and other work ran during most measurements: `RESULTS.md` records the load average around each set. Tables show medians of three to six runs, with the range in brackets where the runs differ. Compare runs made under the same conditions, and measure an app's own views before quoting a number for it: the [load testing guide](/docs/guides/load-testing) shows how.

## Measuring again

Each page ends with the commands that produced its numbers. The writer's and the board's benchmarks run with `go test -bench`; the budget's tests run only when `LITH_BUDGET=1` is set, and write the CSV files the charts read. When a CSV file changes, `go generate ./content` in `site/` draws the charts again, and the site's tests fail until it has.
