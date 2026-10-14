---
title: The shared board
nav: The render loop
headline: 200 streams on one board: the render loop measured
summary: 200 streams on one 139 KB board that changes 800 times a second, in process: one render a tick, no dropped frames, and where the CPU goes.
order: 2
---

`BenchmarkSharedBoard` (`loadtest/bench_test.go`) is kanban's 200-tab run without HTTP: 200 streams watch one board of 200 cards (`loadtest.Board`, 139 KB of HTML), while 200 producers send 800 commands a second between them. Each command adds one to a card's counter, so the board changes in every tick. Each stream compresses every frame and hands it to a client that takes it at once: no network, no load tool. A run warms up for 2 s and is measured for 10 s.

## zstd against brotli

Measured on 2026-10-13 with `synchronous=FULL` and the writer's checkpoints, two sets of three runs; medians of the six, the range in brackets:

| | zstd | brotli |
| --- | --- | --- |
| CPU, cores busy | 1.59 (1.01 to 1.65) | 3.31 (3.00 to 3.61) |
| Ticks a second | 19.8 (17.8 to 19.8) | 19.8 (19.7 to 19.9) |
| Tick p50 | 2.5 ms (1.12 to 3.47) | 0.87 ms (0.75 to 1.69) |
| Tick p99 | 5.2 ms (5.00 to 5.79) | 2.7 ms (2.10 to 3.24) |
| Renders per tick | 1.00 | 1.00 |
| Frames sent a second | 3,954 (3,542 to 3,960) | 3,966 (3,953 to 3,974) |
| Frames dropped | 0 | 0 |
| Bytes per frame on the wire | 2,205 (2,098 to 2,315) | 955 (934 to 1,000) |
| Heap per stream | 1.67 MiB | 2.13 MiB |
| Commands per transaction | 2.1 | 2.0 |

A tick renders the board once for all 200 streams and hands them the frame. No tick overran its 50 ms, and no stream missed a frame.

## Where the CPU goes

In a CPU profile of one run per encoding, compression takes 91% of the samples with brotli and 74% with zstd. Rendering takes 0.3% and 3.6%, the writer 3.5% and 10.6%. Once views render once for every viewer, the encoders are the cost: zstd sends 2.3 times brotli's bytes per frame for half the CPU.

A quarter of zstd's samples are `memmove`, four fifths of them in the encoder's `addBlock`, which copies each frame into its history: the encoder keeps twice its window and, when the next block does not fit, moves the newest half to the front.

zstd's numbers depend on what else the machine does. A zstd process leaves most of its cores idle between ticks, and cores that keep falling idle do the same work in more time, through the CPU's power management or the scheduler moving threads to idle cores. Runs alternated with and without twelve busy loops on the same cores used 0.80 and 0.86 cores with the loops, 1.50 and 1.90 without. brotli keeps three cores busy itself and varies less. Compare runs made under the same conditions.

## FULL against NORMAL

The same benchmark runs each encoding with `synchronous=FULL`, then NORMAL. Measured on 2026-10-13 with Go 1.27.2, two sets of three runs:

```chart
kind: bars
title: A command's time from due to commit, p50, beside 200 streams
x: streams' encoding
y: ms
source: bench/RESULTS.md
---
FULL,zstd,3.4
FULL,brotli,11.0
NORMAL,zstd,0.54
NORMAL,brotli,0.30
```

| | FULL, zstd | NORMAL, zstd | FULL, brotli | NORMAL, brotli |
| --- | --- | --- | --- | --- |
| CPU, cores busy | 0.98 | 1.04 | 2.53 | 2.80 |
| Ticks a second | 19.6 | 19.4 | 15.8 | 19.8 |
| Frames dropped | 0 | 0 | 0 | 0 |
| Commands per transaction | 1.78 | 1.07 | 3.63 | 1.03 |
| Due to commit, p50 | 3.4 ms | 0.54 ms | 11.0 ms | 0.30 ms |

With NORMAL a command commits in about half a millisecond; with FULL it waits for its batch's fsync, 3 to 11 ms, and the writer takes two to four commands into a transaction instead of one. The render loop does the same work in both modes: one render a tick, no dropped frames. With FULL and brotli, the slower commits leave some ticks with nothing new, 15.8 ticks a second against 19.8.

The p99s of these runs are the shared SSD pool's: a run with a disk stall waits for seconds in either mode. `RESULTS.md` has them, with their ranges.

## Measuring again

```sh
taskset -c 0-11 go test ./loadtest -run '^$' -bench SharedBoard -count 3
```

Add `-cpuprofile cpu.out` for the profile. The benchmark logs each run's whole report, and reports its numbers as benchmark metrics for `benchstat`.
