---
title: The shared board
nav: The render loop
headline: 200 streams on one board: the render loop measured
summary: 200 streams on one 139 KB board that changes 800 times a second, in process: one render a tick, no dropped frames, and where the CPU goes.
order: 2
---

`BenchmarkSharedBoard` (`loadtest/bench_test.go`) is kanban's 200-tab run without HTTP: 200 streams watch one board of 200 cards (`loadtest.Board`, 139 KB of HTML), while 200 producers send 800 commands a second between them. Each command adds one to a card's counter, so the board changes in every tick. Each stream compresses every frame and hands it to a client that takes it at once: no network, no load tool. A run warms up for 2 s and is measured for 10 s.

## zstd against brotli

Measured on 2026-10-14 on the development machine with lith v0.2.0, `synchronous=FULL` and the writer's checkpoints, two sets of three runs; medians of the six, the range in brackets:

| | zstd | brotli |
| --- | --- | --- |
| CPU, cores busy | 1.82 (1.28 to 1.90) | 3.42 (3.38 to 3.58) |
| Ticks a second | 19.8 | 19.9 (19.8 to 19.9) |
| Tick p50 | 2.9 ms (0.77 to 3.30) | 1.2 ms (1.04 to 1.37) |
| Tick p99 | 4.1 ms (3.76 to 4.31) | 2.1 ms (2.10 to 2.23) |
| Renders per tick | 1.00 | 1.00 |
| Frames sent a second | 3,960 (3,956 to 3,965) | 3,968 (3,964 to 3,977) |
| Frames dropped | 0 | 0 |
| Bytes per frame on the wire | 2,217 (2,153 to 2,297) | 854 (852 to 862) |
| Heap per stream | 1.68 MiB | 1.56 MiB |
| Commands per transaction | 2.4 | 1.3 |

A tick renders the board once for all 200 streams and hands them the frame. No tick overran its 50 ms, and no stream missed a frame.

## Where the CPU goes

In a CPU profile of one run per encoding, compression takes 94% of the samples with brotli and 76% with zstd. Rendering takes 0.6% and 3.8%, the writer 3.4% and 11.6%. Once views render once for every viewer, the encoders are the cost: zstd sends 2.6 times brotli's bytes per frame for about half the CPU.

A fifth of zstd's samples are `memmove`, three quarters of them in the encoder's `addBlock`, which copies each frame into its history: the encoder keeps twice its window and, when the next block does not fit, moves the newest half to the front.

zstd's numbers depend on what else the machine does. A zstd process leaves most of its cores idle between ticks, and cores that keep falling idle do the same work in more time, through the CPU's power management or the scheduler moving threads to idle cores. On 2026-10-13, runs alternated with and without twelve busy loops on the same cores used 0.80 and 0.86 cores with the loops, 1.50 and 1.90 without. brotli keeps three cores busy itself and varies less. Compare runs made under the same conditions.

## FULL against NORMAL

The same runs measure each encoding with `synchronous=FULL`, then NORMAL:

```chart
kind: bars
title: A command's time from due to commit, p50, beside 200 streams
x: streams' encoding
y: ms
source: bench/RESULTS.md
---
FULL,zstd,5.5
FULL,brotli,2.7
NORMAL,zstd,0.51
NORMAL,brotli,0.29
```

| | FULL, zstd | NORMAL, zstd | FULL, brotli | NORMAL, brotli |
| --- | --- | --- | --- | --- |
| CPU, cores busy | 1.82 | 1.53 | 3.42 | 3.13 |
| Ticks a second | 19.8 | 19.8 | 19.9 | 19.8 |
| Frames dropped | 0 | 0 | 0 | 0 |
| Commands per transaction | 2.39 | 1.03 | 1.30 | 1.02 |
| Due to commit, p50 | 5.5 ms | 0.51 ms | 2.7 ms | 0.29 ms |

With NORMAL a command commits in 0.3 to 0.5 ms; with FULL it waits for its batch's fsync, 2.7 to 5.5 ms, and the writer takes 1.3 to 2.4 commands into a transaction instead of one. The render loop does the same work in both modes: one render a tick, no dropped frames.

The p99s are the shared SSD pool's: medians of 7 to 29 ms in these runs, and seconds in a run with a disk stall, in either mode. `RESULTS.md` has them, with their ranges.

## Streams that do not keep up

In these runs every stream takes every frame. A stream that does not, because its client is slow or its link is ([Streams: slow links](/docs/guides/streams#slow-links)), keeps only the newest frame of each view, and the hub does not render a view while every stream that watches it still has a frame of it waiting ([Ticks](/docs/concepts/ticks#the-newest-frame-wins)). `TestRenderCost` in `live` gives 50 tabs a view of their own of about 34 KB that changes at every tick. Measured on 2026-10-15 on the development machine, three runs each, before the render skip and with it:

| Streams | Renders a second per stream, before | with the skip | Cores busy, before | with the skip |
| --- | --- | --- | --- | --- |
| keep up | 19.8 | 19.8 | 2.17 to 2.20 | 2.17 to 2.19 |
| slow, four frames a second | 19.8 to 19.9 | 3.9 to 4.0 | 1.64 to 1.73 | 0.39 to 0.41 |
| client stopped reading | 19.7 to 19.8 | 0 | 1.42 to 1.65 | 0.03 to 0.05 |

Before, the hub rendered 20 frames a second for a slow stream that sent 4, and for a stream that sent none until its write deadline ended it; it dropped the rest. Now it renders a view again only once a stream has taken the frame before, and `Stats.RendersDeferred` counts the renders it put off. A view that one stream keeps up with renders at every change, for all of its streams.

## Measuring again

```sh
taskset -c 0-11 go test ./loadtest -run '^$' -bench SharedBoard -count 3
LITH_RENDER_COST=1 go test ./live -run TestRenderCost -v -count 3
```

Add `-cpuprofile cpu.out` for the profile. The benchmark logs each run's whole report, and reports its numbers as benchmark metrics for `benchstat`.
