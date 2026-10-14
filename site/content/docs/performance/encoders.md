---
title: Encoders
headline: zstd against brotli: bytes, CPU and heap per stream
summary: zstd at three levels against brotli on the shared board, and the heap each encoder holds per stream: the bytes on the wire, the CPU and the memory each setting trades.
description: zstd at three levels against brotli on the shared board: bytes on the wire per frame, CPU per frame, and the heap each encoder holds for every stream.
order: 3
---

Every stream holds an encoder for as long as its tab is open, so an encoder costs three things: bytes on the wire per frame, CPU per frame, and heap per stream. `sse.Options.ZstdLevel`, `ZstdWindow`, `ZstdLowMemory`, `BrotliQuality` and `BrotliWindow` choose among them.

## On the shared board

`BenchmarkBoardZstdLevels` is [the shared board](/docs/performance/render-loop) with every stream on zstd, at the fastest level (lith's default), the default level and the better-compression level. Measured on 2026-10-14 on the development machine with lith v0.2.0, medians of three runs; brotli from `BenchmarkSharedBoard` the same day:

```chart
kind: bars
title: Bytes per frame of the 140 KB board, 200 streams
x: encoder
y: bytes per frame
source: bench/RESULTS.md
---
bytes per frame,zstd fastest,2213
bytes per frame,zstd default,1087
bytes per frame,zstd better,365
bytes per frame,brotli q4,854
```

```chart
kind: bars
title: Cores busy in the same runs
x: encoder
y: cores
source: bench/RESULTS.md
---
cores busy,zstd fastest,1.84
cores busy,zstd default,1.74
cores busy,zstd better,3.15
cores busy,brotli q4,3.42
```

| Encoder | Bytes per frame | Cores busy | Heap per stream | Tick p99 |
| --- | --- | --- | --- | --- |
| zstd fastest (lith's default) | 2,213 (2,186 to 2,220) | 1.84 (1.80 to 1.90) | 1.66 MiB | 4.1 ms |
| zstd default | 1,087 (1,076 to 1,129) | 1.74 (1.71 to 1.76) | 2.76 MiB | 4.2 ms |
| zstd better compression | 365 (364 to 367) | 3.15 (3.01 to 3.45) | 5.56 MiB | 3.3 ms |
| brotli quality 4 | 854 (852 to 862) | 3.42 (3.38 to 3.58) | 1.56 MiB | 2.1 ms |

- zstd's default level halves the bytes of the fastest for no more CPU in these runs, and holds 1.1 MiB more per stream: about 220 MiB more for 200 tabs.
- Better compression sends a third of the default's bytes, fewer than brotli, for about brotli's CPU and three and a half times its heap.
- Every run sent 3,959 to 3,977 frames a second and dropped none.

## Heap per stream

`TestHeapPerStream` (`sse/heap_test.go`) measures the encoders on one core, without the render loop: 200 frames of a 57 KB page, of which five rows change from one frame to the next, to each of 20 streams. Three runs on 2026-10-14 on the development machine gave the same bytes each time:

| Encoder | Bytes per frame | Time per frame | Heap per stream |
| --- | --- | --- | --- |
| zstd fastest | 405 | 29 to 30 µs | 1.65 MiB |
| zstd fastest, `ZstdLowMemory` | 405 | 40 to 42 µs | 1.03 MiB |
| zstd default | 228 | 23 to 25 µs | 2.73 MiB |
| zstd better | 109 | 119 to 122 µs | 5.54 MiB |
| brotli quality 4 | 155 | 249 to 257 µs | 1.55 MiB |

- zstd keeps a history of twice its window, 1 MiB for the 512 KiB window. `ZstdLowMemory` keeps it at the window: 0.62 MiB less per stream, for 40% more CPU per frame here.
- Datastar's Go SDK, given `WithBrotli` without `WithBrotliLGWin`, keeps brotli's 4 MiB window: 9.9 MiB of heap per stream, measured the same way at quality 5. 1,000 open tabs hold about 9.7 GiB there, against 1.0 GiB with zstd and `ZstdLowMemory`, or 1.6 GiB with lith's default.

## For slow links

A stream to a client on a slow link (`sse.Options.Slow`, which `live.Spec.Slow` sets) takes zstd's default level while its budget has room for it, and brotli's quality 4 where another stream would drop to 3 ([Streams: slow links](/docs/guides/streams#slow-links)). `TestSlowLinks` in `bench/budget` (`slow.csv`) measures both on the budget's workloads, the way the budget's tiers were measured: 64 streams, the heap they keep, the process's CPU per frame. On 2026-10-14 on the development machine, zstd at 2^19, the tier a stream gets while the budget has room:

| Frames | Bytes a frame, fastest | Default level | CPU a frame, fastest | Default level |
| --- | --- | --- | --- | --- |
| board of 50 KB, a card changed | 287 | 266 | 18.6 µs | 20.8 µs |
| board of 142 KB | 1,471 | 1,347 | 61.6 µs | 78.0 µs |
| board of 230 KB | 2,948 | 2,810 | 103 µs | 127 µs |
| table of 230 KB, a few cells changed | 4,598 | 4,436 | 122 µs | 178 µs |
| list of 230 KB, 13 rows of 1,300 changed | 1,738 | 807 | 104 µs | 86.6 µs |
| a business app's tab, 2 to 37 KB | 189 | 176 | 8.2 µs | 9.9 µs |
| Game of Life, 45 KB | 601 | 608 | 32.5 µs | 32.7 µs |

- The default level pays where a frame changes in many places: half the bytes on the list. Where it changes in one place it saves 4 to 8%, and on the Game of Life nothing.
- It holds 1.0 to 1.1 MiB more at every window: up to 2.8 MiB at 2^19, 2.0 MiB at 2^18 with low memory. The budget books those tiers at what they held.
- Its CPU a frame ranges from 17% less (the list) to 46% more (the table). A slow stream also gets about four frames a second instead of ten or twenty.
- Brotli at quality 4 instead of 3, at the window that holds the frame, sends 29 to 46% fewer bytes on the boards, the table and the list, 9% on the business app's tab and 3% on the Game of Life, for 0.5 MiB more heap and 3% less to 16% more CPU.

## Choosing

- Memory is the constraint for most apps: an open tab holds its encoder for as long as it stays open. The [stream budget](/docs/performance/budget) decides the window per stream from the memory there is and the size of the stream's frames.
- lith's default is zstd's fastest level at 2^19, 1.65 MiB a stream. Where bandwidth costs more than memory, `ZstdLevel: zstd.SpeedDefault` halves the bytes for 1.1 MiB more a stream. The budget's tiers are sized for the fastest level, though: at another level a stream holds more than the budget reserves for it, so leave room under the limit. A slow stream's default level is booked at its own size.
- brotli serves the browsers without zstd, Safari before 26.3 among them. Its CPU per frame is eight to nine times zstd's in the table above, and five to seven and a half times on the 142 KB board at the same window ([the stream budget](/docs/performance/budget)).

## Measuring again

```sh
taskset -c 0-11 go test ./loadtest -run '^$' -bench BoardZstdLevels -count 3
go test ./sse -run TestHeapPerStream -v -count 3
LITH_BUDGET=1 taskset -c 0-11 go test ./bench/budget -run SlowLinks -v -timeout 1h
```
