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

## Choosing

- Memory is the constraint for most apps: an open tab holds its encoder for as long as it stays open. The [stream budget](/docs/performance/budget) decides the window per stream from the memory there is and the size of the stream's frames.
- lith's default is zstd's fastest level at 2^19, 1.65 MiB a stream. Where bandwidth costs more than memory, `ZstdLevel: zstd.SpeedDefault` halves the bytes for 1.1 MiB more a stream. The budget's tiers are sized for the fastest level, though: at another level a stream holds more than the budget reserves for it, so leave room under the limit.
- brotli serves the browsers without zstd, Safari before 26.3 among them. Its CPU per frame is eight to nine times zstd's in the table above, and five to seven and a half times on the 142 KB board at the same window ([the stream budget](/docs/performance/budget)).

## Measuring again

```sh
taskset -c 0-11 go test ./loadtest -run '^$' -bench BoardZstdLevels -count 3
go test ./sse -run TestHeapPerStream -v -count 3
```
