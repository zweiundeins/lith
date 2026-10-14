---
title: The stream budget
nav: The budget
headline: The stream budget: encoder memory as streams grow
summary: What each window and encoder setting costs on two workloads, what a stream holds at the least, how budgets behave as up to 2,000 streams open, and the defaults lith took from it.
description: What each window and encoder setting costs on two workloads, the least a stream holds, and how budgets behave as up to 2,000 streams open.
order: 4
---

The tests in `bench/budget` measured what a stream's encoder costs under each setting, on two workloads, and simulated memory budgets as streams open. Their CSV files are the source of the charts on this page. Measured on 2026-10-13 with Go 1.27.2, klauspost/compress v1.20.3 and andybalholm/brotli v1.2.6, pinned to the performance cores.

The two workloads:

- **The board**: `loadtest.Board`'s view rendered from SQLite, 200 frames of 142 KB, one card changed before each.
- **The page**: a synthetic tab of a small business app, shaped like libretto's: lists of up to 72 entries, a form in a modal, a detail page and an overview, 150 frames of 2.4 to 36.9 KB (13.6 KB on average), most of them a view the tab saw before, with a change.

## Settings

`TestSettings` writes each workload's frames to 64 streams in turn, as the hub does, and measures the heap the open streams keep after two collections, the bytes per frame on the wire after the first frame, and the CPU time per frame: the median of three runs, which differ by under 6%.

```chart
kind: bars
title: Bytes per frame of the 142 KB board, by window
x: window
x-prefix: 2^
y: bytes per frame
source: bench/budget/settings.csv
columns: coding, window, bytes_per_frame
where: workload=board, low_memory=false, quality!=3
series: zstd, fastest=zstd; brotli, quality 4=br
---
"zstd, fastest",15,11234
"zstd, fastest",16,10872
"zstd, fastest",17,10831
"zstd, fastest",18,2703
"zstd, fastest",19,1471
"zstd, fastest",20,794
"brotli, quality 4",16,9716
"brotli, quality 4",17,9648
"brotli, quality 4",18,986
"brotli, quality 4",19,389
"brotli, quality 4",20,161
```

A window that cannot reach the frame before costs the board four times the bytes and over twice the CPU: below 2^18, a frame finds the previous one out of reach. The page loses less, since a window still holds several of its smaller frames: against 2^19, it sends twice the bytes at 2^17, three times at 2^16 and six times at 2^15. libretto's real frames lose less again, 9% more at 2^17 and 19% at 2^16, since its sessions repeat a few views more often than this page.

```chart
kind: bars
title: Heap per stream with zstd, by window
x: window
x-prefix: 2^
y: MiB per stream
source: bench/budget/settings.csv
columns: low_memory, window, heap_mib_per_stream
where: workload=board, coding=zstd
series: full history=false; low memory=true
---
full history,15,1.622
full history,16,1.657
full history,17,1.656
full history,18,1.656
full history,19,1.656
full history,20,2.656
low memory,15,0.525
low memory,16,0.626
low memory,17,0.688
low memory,18,0.813
low memory,19,1.063
low memory,20,1.563
```

zstd with its full history holds 1.6 MiB at every window up to 2^19, so below 2^19 only `ZstdLowMemory` saves heap. Low memory costs CPU with the board's large frames, 84 µs per frame against 62 at 2^19 and 124 against 56 at 2^20, and 1 to 2 µs with the page's. On the board, brotli at quality 3 holds 0.86 MiB less than at quality 4 for the same window and sends about twice its bytes; both take four to six times zstd's CPU. The table of every setting, for both workloads, is in [`bench/RESULTS.md`](https://github.com/zweiundeins/lith/blob/main/bench/RESULTS.md) and `settings.csv`.

## The least a stream holds

`TestFloor` (`floor.csv`, the page) finds what a compressed stream holds whatever its window: zstd with low memory 0.44 MiB at its smallest window, 2^10 (its 256 KiB hash table, 129 KiB of history and its block buffers), brotli at quality 2 or 3 0.60 MiB at windows up to 2^14, gzip 0.79 MiB. Below 0.44 MiB, a stream only goes out uncompressed.

## A budget as streams grow

`TestSimulation` (`simulation.csv`) opens up to 2,000 streams of one workload and, after every ten, adds up the measured heap and bytes per frame of what each stream holds. The budgets are `sse.Budget` with `MinWindow` at the workload's floor (2^18 for the board, 2^17 for the page) and `MaxWindow` 2^19. In all but one policy, every open stream writes after every ten opens, so that tiers shrink and waiting streams take the room that frees up.

```chart
kind: lines
title: Heap the encoders hold as board streams open
x: open streams
y: MiB
source: bench/budget/simulation.csv
columns: policy, open, encoder_heap_mib
where: workload=board
series: no limit, zstd 2^19=lith default; every stream at zstd 2^18 low memory=static low memory; budget of 1 GiB=budget of 1 GiB; budget of 128 MiB=budget
step: 100
---
"no limit, zstd 2^19",0,0.0
"no limit, zstd 2^19",100,165.6
"no limit, zstd 2^19",200,331.2
"no limit, zstd 2^19",300,496.8
"no limit, zstd 2^19",400,662.4
"no limit, zstd 2^19",500,828.0
"no limit, zstd 2^19",600,993.6
"no limit, zstd 2^19",700,1159.2
"no limit, zstd 2^19",800,1324.8
"no limit, zstd 2^19",900,1490.4
"no limit, zstd 2^19",1000,1656.0
"no limit, zstd 2^19",1100,1821.6
"no limit, zstd 2^19",1200,1987.2
"no limit, zstd 2^19",1300,2152.8
"no limit, zstd 2^19",1400,2318.4
"no limit, zstd 2^19",1500,2484.0
"no limit, zstd 2^19",1600,2649.6
"no limit, zstd 2^19",1700,2815.2
"no limit, zstd 2^19",1800,2980.8
"no limit, zstd 2^19",1900,3146.4
"no limit, zstd 2^19",2000,3312.0
every stream at zstd 2^18 low memory,0,0.0
every stream at zstd 2^18 low memory,100,81.3
every stream at zstd 2^18 low memory,200,162.6
every stream at zstd 2^18 low memory,300,243.9
every stream at zstd 2^18 low memory,400,325.2
every stream at zstd 2^18 low memory,500,406.5
every stream at zstd 2^18 low memory,600,487.8
every stream at zstd 2^18 low memory,700,569.1
every stream at zstd 2^18 low memory,800,650.4
every stream at zstd 2^18 low memory,900,731.7
every stream at zstd 2^18 low memory,1000,813.0
every stream at zstd 2^18 low memory,1100,894.3
every stream at zstd 2^18 low memory,1200,975.6
every stream at zstd 2^18 low memory,1300,1056.9
every stream at zstd 2^18 low memory,1400,1138.2
every stream at zstd 2^18 low memory,1500,1219.5
every stream at zstd 2^18 low memory,1600,1300.8
every stream at zstd 2^18 low memory,1700,1382.1
every stream at zstd 2^18 low memory,1800,1463.4
every stream at zstd 2^18 low memory,1900,1544.7
every stream at zstd 2^18 low memory,2000,1626.0
budget of 1 GiB,0,0.0
budget of 1 GiB,100,165.6
budget of 1 GiB,200,331.2
budget of 1 GiB,300,496.8
budget of 1 GiB,400,662.4
budget of 1 GiB,500,828.0
budget of 1 GiB,600,993.6
budget of 1 GiB,700,744.1
budget of 1 GiB,800,850.4
budget of 1 GiB,900,956.7
budget of 1 GiB,1000,813.0
budget of 1 GiB,1100,894.3
budget of 1 GiB,1200,975.6
budget of 1 GiB,1300,1004.9
budget of 1 GiB,1400,1005.0
budget of 1 GiB,1500,1005.1
budget of 1 GiB,1600,1005.2
budget of 1 GiB,1700,1005.3
budget of 1 GiB,1800,1005.4
budget of 1 GiB,1900,1005.5
budget of 1 GiB,2000,1005.6
budget of 128 MiB,0,0.0
budget of 128 MiB,100,106.3
budget of 128 MiB,200,125.2
budget of 128 MiB,300,125.3
budget of 128 MiB,400,125.4
budget of 128 MiB,500,125.5
budget of 128 MiB,600,125.6
budget of 128 MiB,700,125.7
budget of 128 MiB,800,125.8
budget of 128 MiB,900,125.9
budget of 128 MiB,1000,126.0
budget of 128 MiB,1100,126.1
budget of 128 MiB,1200,126.2
budget of 128 MiB,1300,126.3
budget of 128 MiB,1400,126.4
budget of 128 MiB,1500,126.5
budget of 128 MiB,1600,126.6
budget of 128 MiB,1700,126.7
budget of 128 MiB,1800,126.8
budget of 128 MiB,1900,126.9
budget of 128 MiB,2000,127.0
```

```chart
kind: lines
title: Streams that compress as board streams open
x: open streams
y: compressed streams
source: bench/budget/simulation.csv
columns: policy, open, compressed
where: workload=board
series: no limit, zstd 2^19=lith default; budget of 1 GiB=budget of 1 GiB; budget of 128 MiB=budget; budget of 128 MiB, at open only=budget at open
step: 100
---
"no limit, zstd 2^19",0,0
"no limit, zstd 2^19",100,100
"no limit, zstd 2^19",200,200
"no limit, zstd 2^19",300,300
"no limit, zstd 2^19",400,400
"no limit, zstd 2^19",500,500
"no limit, zstd 2^19",600,600
"no limit, zstd 2^19",700,700
"no limit, zstd 2^19",800,800
"no limit, zstd 2^19",900,900
"no limit, zstd 2^19",1000,1000
"no limit, zstd 2^19",1100,1100
"no limit, zstd 2^19",1200,1200
"no limit, zstd 2^19",1300,1300
"no limit, zstd 2^19",1400,1400
"no limit, zstd 2^19",1500,1500
"no limit, zstd 2^19",1600,1600
"no limit, zstd 2^19",1700,1700
"no limit, zstd 2^19",1800,1800
"no limit, zstd 2^19",1900,1900
"no limit, zstd 2^19",2000,2000
budget of 1 GiB,0,0
budget of 1 GiB,100,100
budget of 1 GiB,200,200
budget of 1 GiB,300,300
budget of 1 GiB,400,400
budget of 1 GiB,500,500
budget of 1 GiB,600,600
budget of 1 GiB,700,700
budget of 1 GiB,800,800
budget of 1 GiB,900,900
budget of 1 GiB,1000,1000
budget of 1 GiB,1100,1100
budget of 1 GiB,1200,1200
budget of 1 GiB,1300,1236
budget of 1 GiB,1400,1236
budget of 1 GiB,1500,1236
budget of 1 GiB,1600,1236
budget of 1 GiB,1700,1236
budget of 1 GiB,1800,1236
budget of 1 GiB,1900,1236
budget of 1 GiB,2000,1236
budget of 128 MiB,0,0
budget of 128 MiB,100,100
budget of 128 MiB,200,154
budget of 128 MiB,300,154
budget of 128 MiB,400,154
budget of 128 MiB,500,154
budget of 128 MiB,600,154
budget of 128 MiB,700,154
budget of 128 MiB,800,154
budget of 128 MiB,900,154
budget of 128 MiB,1000,154
budget of 128 MiB,1100,154
budget of 128 MiB,1200,154
budget of 128 MiB,1300,154
budget of 128 MiB,1400,154
budget of 128 MiB,1500,154
budget of 128 MiB,1600,154
budget of 128 MiB,1700,154
budget of 128 MiB,1800,154
budget of 128 MiB,1900,154
budget of 128 MiB,2000,154
"budget of 128 MiB, at open only",0,0
"budget of 128 MiB, at open only",100,77
"budget of 128 MiB, at open only",200,77
"budget of 128 MiB, at open only",300,77
"budget of 128 MiB, at open only",400,77
"budget of 128 MiB, at open only",500,77
"budget of 128 MiB, at open only",600,77
"budget of 128 MiB, at open only",700,77
"budget of 128 MiB, at open only",800,77
"budget of 128 MiB, at open only",900,77
"budget of 128 MiB, at open only",1000,77
"budget of 128 MiB, at open only",1100,77
"budget of 128 MiB, at open only",1200,77
"budget of 128 MiB, at open only",1300,77
"budget of 128 MiB, at open only",1400,77
"budget of 128 MiB, at open only",1500,77
"budget of 128 MiB, at open only",1600,77
"budget of 128 MiB, at open only",1700,77
"budget of 128 MiB, at open only",1800,77
"budget of 128 MiB, at open only",1900,77
"budget of 128 MiB, at open only",2000,77
```

At 2,000 streams:

| Policy | Board: compressed | heap | bytes per frame | Page: compressed | heap | bytes per frame |
| --- | --- | --- | --- | --- | --- | --- |
| no limit, zstd 2^19 | 2,000 | 3,312 MiB | 1,471 | 2,000 | 3,294 MiB | 189 |
| zstd at the floor, low memory | 2,000 | 1,626 MiB | 2,703 | 2,000 | 1,278 MiB | 379 |
| budget of 128 MiB, at open only | 77 | 129 MiB | 136,654 | 77 | 129 MiB | 13,183 |
| budget of 128 MiB | 154 | 127 MiB | 131,336 | 182 | 118 MiB | 12,491 |
| budget of 128 MiB, 503 | 154, 1,846 refused | 125 MiB | 2,703 | 182, 1,818 refused | 116 MiB | 379 |
| budget of 1 GiB | 1,236 | 1,006 MiB | 55,940 | 1,456 | 931 MiB | 4,003 |

- At open only, the first 77 streams take zstd 2^19 and leave no room for the rest. When streams that find no room make the others shrink, the budget holds as many streams as fit at the floor.
- A budget of 1 GiB gives the board's streams zstd 2^19 up to about 600 streams, 2^19 with low memory up to about 950, 2^18 with low memory up to 1,236, and nothing beyond.
- With `Refuse`, the streams beyond the floor's room get a 503 instead of every frame uncompressed: the bytes per frame of the streams that run stay those of the floor.

## The board with and without a budget

`TestLoadtest` (`loadtest.csv`) runs the shared board in process while 200 producers change cards 800 times a second, about 40 cards per frame. Two runs each:

| Streams | Policy | Cores busy | Bytes per frame | Heap | Frames dropped |
| --- | --- | --- | --- | --- | --- |
| 200 | lith's default | 1.55, 1.63 | 2,177, 2,239 | 334 MiB | 0 |
| 200 | zstd 2^18, low memory | 1.81, 1.89 | 3,984, 3,881 | 164 MiB | 0 |
| 200 | budget of 128 MiB: 154 at zstd 2^18 low memory, 46 raw | 1.48, 1.47 | 35,718, 35,864 | 126 MiB | 0 |
| 1,000 | lith's default | 4.06, 4.13 | 2,226, 2,171 | 1,660 MiB | 0 |
| 1,000 | zstd 2^18, low memory | 4.52, 4.38 | 4,040, 3,996 | 816 MiB | 0 |
| 1,000 | budget of 128 MiB: 154 compressed, 846 raw | 2.11, 1.95 | 121,272, 121,288 | 129 MiB | 0 |

Every run held about 19.8 ticks a second. The streams the budget had no room for sent 142 KB per frame, 20 frames a second: with `Refuse` they would have been answered 503 instead.

## Lowering the limit

`TestPressure` (`pressure.csv`): 500 board streams hold zstd 2^19 in a budget of 1 GiB, 828 MiB. After `SetLimit(512 << 20)`, each moves to 2^18 with low memory at its next frame: 414 MiB reserved, and 407 MiB of live heap after a collection, from 828. That frame starts without history and takes 10,925 bytes.

## What lith took from it

Since 2026-10-13, a hub makes a budget when it is given none: half the process's memory limit, windows from 2^19 down to 2^18, streams uncompressed when even 2^18 does not fit, and no change at all without a memory limit. A stream's own `ZstdWindow` and `BrotliWindow` cap what it gets. Pages with large frames pass a budget with `Refuse`, and hibernation is opt-in ([Hibernation](/docs/performance/hibernation)). Brotli streams cannot move down once open, so they step down as they open instead, below `MinWindow` to quality 3 at 2^17 and 2^16. Past 20 open streams, a brotli stream takes at most its largest tier times twice the share of the limit that is left. `TestBudgetCurve` in `sse` opens streams one by one, with a write to each after every ten opens; brotli at 2^19 and quality 4, zstd at 2^19:

| Budget | Streams | Full tier at open | Compressed, br/zstd | First br uncompressed | Before the curve |
| --- | --- | --- | --- | --- | --- |
| 64 MiB | 128 br | 20 | 32/0 | 33rd | 24 compressed |
| 128 MiB | 256 br | 25 | 79/0 | 80th | 49 compressed |
| 150 MiB | 300 br | 29 | 93/0 | 94th | 57 compressed |
| 150 MiB | 300 zstd | 90 | 0/181 | none | the same |
| 150 MiB | 150 br, 150 zstd | 62 | 56/74 | 88th | 38/69 |

zstd streams keep the fair share they had: they move down while open, so the curve would only cost them. [Streams and compression](/docs/guides/streams#the-memory-budget) shows the settings.

## Measuring again

The tests run only with `LITH_BUDGET=1`, and write the CSV files next to them:

```sh
LITH_BUDGET=1 LITH_BUDGET_RUNS=3 taskset -c 0-11 go test ./bench/budget -run Settings -v -timeout 1h
```

`-run` picks the test: `Settings`, `Floor`, `Simulation`, `Hibernation`, `WakeCost`, `Loadtest` or `Pressure`. After a run, `go generate ./content` in `site/` draws this page's charts from the new files.
