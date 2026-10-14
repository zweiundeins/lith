---
title: The stream budget
nav: The budget
headline: The stream budget: what streams hold as they grow
summary: What each window and encoder setting costs, what a stream and a view hold besides the encoder, how budgets behave as streams open, and the defaults lith took from it.
description: What each window and encoder setting costs, what streams and views hold besides their encoders, and how budgets behave as streams open.
order: 4
---

The tests in `bench/budget` measured what a stream's encoder costs under each setting, what a stream and a view hold besides it, and how memory budgets behave as streams open. Their CSV files are the source of the charts and tables on this page. Measured on 2026-10-13 and 2026-10-14 with Go 1.27.2, klauspost/compress v1.20.3 and andybalholm/brotli v1.2.6, pinned to the performance cores.

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

## Large frames and brotli's flushes

`TestFrameSizes` (`framesize.csv`, measured on 2026-10-14) writes frames of about 50, 142 and 230 KB to 64 streams of each tier: the board with 70, 200 and 330 cards; a data table of 690 rows that changes three cells before each frame and scrolls a few rows every tenth; and a list of 1,300 rows of which every hundredth changes in place. zstd holds what its tier books at every size, or at most 40 KiB more.

A brotli encoder that is not flushed codes all it took since its last flush as one metablock, and keeps an output buffer twice its size: its heap grew with the frame. Within a metablock it also extends a copy into the next input block (16 KiB at quality 3, 64 KiB at 4) without hashing that block, so after a change it could find nothing of the frame before and coded the rest of the frame in short pieces of itself. A stream now flushes its brotli encoder every 16 KiB of a frame at quality 3 and every 32 KiB at 4: it parses and hashes every block, and holds the same heap at every frame size, 12 to 32% below its tier. At 2^18, before and after:

| Frame | Setting | Heap | Bytes per frame | CPU per frame |
| --- | --- | --- | --- | --- |
| board, 230 KB | quality 3 | 1,554 KiB, now 1,083 | 3,173, now 259 | 559 µs, now 630 |
| board, 230 KB | quality 4 | 2,370 KiB, now 1,603 | 2,382, now 140 | 625 µs, now 648 |
| data table, 230 KB | quality 3 | 1,562 KiB, now 1,091 | 5,756, now 227 | 704 µs, now 627 |
| data table, 230 KB | quality 4 | 2,373 KiB, now 1,611 | 3,450, now 134 | 734 µs, now 637 |
| list, 230 KB | quality 3 | 1,530 KiB, now 1,063 | 363, now 676 | 511 µs, now 666 |
| list, 230 KB | quality 4 | 2,346 KiB, now 1,591 | 285, now 406 | 590 µs, now 656 |
| board, 50 KB | quality 4 | 1,790 KiB, now 1,585 | 34, now 50 | 137 µs, now 141 |

Each flush costs a metablock header, about 25 bytes when the block holds a change: the list, with its changes spread through every block, pays that and loses what the flushes win elsewhere. A window smaller than the frame gains nothing from them and pays up to 5% more bytes.

A frame refers to the one before while the window holds it. `TestWindowMargin` (`margin.csv`) writes data tables of 206 to 266 KB at 2^18: at 259 KB, 99% of the window, brotli at quality 3 still takes 249 bytes a frame, and at 266 KB 29,719. zstd with low memory takes 5,232 bytes at 206 KB, 7,742 at 259 KB and 29,510 at 266 KB: it finds less of the frame before as the window fills.

## What a stream holds besides its encoder

`TestViewCost` (`viewcost.csv`) streams data tables of 0.8, 49, 143 and 233 KB from a live hub to 64 streams, which watch a view each, as tabs with a view of their own do, or one view they share. After a collection:

- A view holds its newest frame, rounded up to the heap's pages of 8 KiB: 56 KiB for the 49 KB table, 144 KiB for 143 KB, 232 KiB for 233 KB, and about 1 KiB besides.
- A stream holds 3 to 7 KiB of heap in the hub, and 8 to 10 KiB of stack. net/http holds 14 KiB of heap and 8 KiB of stack for an open HTTP/1.1 connection.
- A render of the 233 KB table allocates 1,046 KiB: the buffer it grows, the event it formats, and the formatting. The next collection frees it.

In a hub, a stream with an encoder holds what `TestFrameSizes` measured for it: with the 233 KB table shared, 1,078 KiB a stream at quality 3 and 2^18, 1,605 KiB at quality 4 and 850 KiB with zstd at 2^18 and low memory, the hub's part included.

So a budget books what stays: each stream's encoder at its tier, the most a stream held at that tier with frames of 50 to 230 KB rounded up to 16 KiB; 32 KiB for each stream; and each view's newest frame at its length and 8 KiB, once for all the streams that watch it. A tab with a 233 KB view of its own at quality 3 and 2^18 books 1,104 + 32 + 235 = 1,371 KiB, the view a sixth of it. `TestBudgetMatchesHeap` in `live` checks the model: for 12 streams with a view of 146 or 246 KB each, what the budget books is what they hold after a collection, within 1 to 4% for brotli and zstd and 5 to 8% uncompressed, where the 32 KiB include net/http's buffers that the test's streams have none of.

What a render allocates is garbage: it is not part of what the streams hold, and the budget does not book it. Go's collector lets the heap grow to twice what is live before it collects (GOGC=100), and the budget's default limit, half of GOMEMLIMIT, leaves that half to the garbage and to the rest of the app. A tab that renders its own 233 KB view every tick makes 1 MiB of garbage a tick, and 150 such tabs 150 MiB.

## A budget as streams grow

`TestSimulation` (`simulation.csv`) opens up to 2,000 streams of one workload and, after every ten, adds up the measured heap and bytes per frame of what each stream holds. The budgets are `sse.Budget` with `MaxWindow` 2^19, and with `MinWindow` at the workload's floor (2^18 for the board, 2^17 for the page), since the simulated writes are small. Each stream writes once when it opens, which picks its tier; in all but one policy every open stream also writes after every ten opens, so that tiers shrink and waiting streams take the room that frees up.

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
budget of 1 GiB,600,637.3
budget of 1 GiB,700,744.1
budget of 1 GiB,800,850.4
budget of 1 GiB,900,731.4
budget of 1 GiB,1000,813.0
budget of 1 GiB,1100,894.3
budget of 1 GiB,1200,934.7
budget of 1 GiB,1300,934.8
budget of 1 GiB,1400,934.9
budget of 1 GiB,1500,935.0
budget of 1 GiB,1600,935.1
budget of 1 GiB,1700,935.2
budget of 1 GiB,1800,935.3
budget of 1 GiB,1900,935.4
budget of 1 GiB,2000,935.5
budget of 128 MiB,0,0.0
budget of 128 MiB,100,106.3
budget of 128 MiB,200,116.8
budget of 128 MiB,300,116.9
budget of 128 MiB,400,117.0
budget of 128 MiB,500,117.1
budget of 128 MiB,600,117.2
budget of 128 MiB,700,117.3
budget of 128 MiB,800,117.4
budget of 128 MiB,900,117.5
budget of 128 MiB,1000,117.6
budget of 128 MiB,1100,117.7
budget of 128 MiB,1200,117.8
budget of 128 MiB,1300,117.9
budget of 128 MiB,1400,118.0
budget of 128 MiB,1500,118.1
budget of 128 MiB,1600,118.2
budget of 128 MiB,1700,118.3
budget of 128 MiB,1800,118.4
budget of 128 MiB,1900,118.5
budget of 128 MiB,2000,118.6
```

```chart
kind: lines
title: Streams that compress as board streams open
x: open streams
y: compressed streams
source: bench/budget/simulation.csv
columns: policy, open, compressed
where: workload=board
series: no limit, zstd 2^19=lith default; budget of 1 GiB=budget of 1 GiB; budget of 128 MiB=budget; budget of 128 MiB, first write only=budget at first write
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
budget of 1 GiB,1200,1150
budget of 1 GiB,1300,1150
budget of 1 GiB,1400,1150
budget of 1 GiB,1500,1150
budget of 1 GiB,1600,1150
budget of 1 GiB,1700,1150
budget of 1 GiB,1800,1150
budget of 1 GiB,1900,1150
budget of 1 GiB,2000,1150
budget of 128 MiB,0,0
budget of 128 MiB,100,100
budget of 128 MiB,200,144
budget of 128 MiB,300,144
budget of 128 MiB,400,144
budget of 128 MiB,500,144
budget of 128 MiB,600,144
budget of 128 MiB,700,144
budget of 128 MiB,800,144
budget of 128 MiB,900,144
budget of 128 MiB,1000,144
budget of 128 MiB,1100,144
budget of 128 MiB,1200,144
budget of 128 MiB,1300,144
budget of 128 MiB,1400,144
budget of 128 MiB,1500,144
budget of 128 MiB,1600,144
budget of 128 MiB,1700,144
budget of 128 MiB,1800,144
budget of 128 MiB,1900,144
budget of 128 MiB,2000,144
"budget of 128 MiB, first write only",0,0
"budget of 128 MiB, first write only",100,74
"budget of 128 MiB, first write only",200,74
"budget of 128 MiB, first write only",300,74
"budget of 128 MiB, first write only",400,74
"budget of 128 MiB, first write only",500,74
"budget of 128 MiB, first write only",600,74
"budget of 128 MiB, first write only",700,74
"budget of 128 MiB, first write only",800,74
"budget of 128 MiB, first write only",900,74
"budget of 128 MiB, first write only",1000,74
"budget of 128 MiB, first write only",1100,74
"budget of 128 MiB, first write only",1200,74
"budget of 128 MiB, first write only",1300,74
"budget of 128 MiB, first write only",1400,74
"budget of 128 MiB, first write only",1500,74
"budget of 128 MiB, first write only",1600,74
"budget of 128 MiB, first write only",1700,74
"budget of 128 MiB, first write only",1800,74
"budget of 128 MiB, first write only",1900,74
"budget of 128 MiB, first write only",2000,74
```

At 2,000 streams:

| Policy | Board: compressed | heap | bytes per frame | Page: compressed | heap | bytes per frame |
| --- | --- | --- | --- | --- | --- | --- |
| no limit, zstd 2^19 | 2,000 | 3,312 MiB | 1,471 | 2,000 | 3,294 MiB | 189 |
| zstd at the floor, low memory | 2,000 | 1,626 MiB | 2,703 | 2,000 | 1,278 MiB | 379 |
| budget of 128 MiB, first write only | 74 | 124 MiB | 136,865 | 74 | 123 MiB | 13,203 |
| budget of 128 MiB | 144 | 119 MiB | 132,037 | 167 | 109 MiB | 12,590 |
| budget of 128 MiB, 503 | 144, 1,856 refused | 117 MiB | 2,762 | 167, 1,833 refused | 107 MiB | 379 |
| budget of 1 GiB | 1,150 | 936 MiB | 61,937 | 1,337 | 855 MiB | 4,796 |

- When each stream writes only its first frame, the first 73 take zstd 2^19 and leave no room for the rest. When streams that find no room make the others shrink, the budget holds as many streams as fit at the floor.
- A budget of 1 GiB gives the board's streams zstd 2^19 up to about 550 streams, 2^19 with low memory up to about 850, 2^18 with low memory up to 1,150, and nothing beyond. A stream that finds room for 2^15 but not for 2^18 takes 2^15, which compresses each frame alone: one stream at a time.
- The budget books 32 KiB for every open stream, compressed or not: 62 MiB at 2,000 streams, beyond the limit when the streams cannot be turned away.
- With `Refuse`, the streams beyond the floor's room get a 503 instead of every frame uncompressed: the bytes per frame of the streams that run stay near those of the floor.

## The board with and without a budget

`TestLoadtest` (`loadtest.csv`) runs the shared board in process while 200 producers change cards 800 times a second, about 40 cards per frame. Two runs each:

| Streams | Policy | Cores busy | Bytes per frame | Heap | Frames dropped |
| --- | --- | --- | --- | --- | --- |
| 200 | lith's default | 1.74, 1.65 | 2,285, 2,199 | 334 MiB | 0 |
| 200 | zstd 2^18, low memory | 1.89, 1.89 | 3,908, 3,990 | 164 MiB | 0 |
| 200 | budget of 128 MiB: 141 at zstd 2^18 low memory, 59 raw | 1.64, 1.81 | 44,776, 44,789 | 116 MiB | 0 |
| 1,000 | lith's default | 3.80, 3.77 | 2,198, 2,221 | 1,668 MiB | 0 |
| 1,000 | zstd 2^18, low memory | 4.30, 4.32 | 3,984, 3,984 | 816 MiB | 0 |
| 1,000 | budget of 128 MiB: 124 and 123 compressed, the rest raw | 1.85, 1.82 | 125,434, 125,572 | 104 MiB | 0 |

Every run held about 19.8 ticks a second. The budget learned the board's 142 KB frame from the streams' first writes and gave no stream less than 2^18. With 1,000 streams, the 32 KiB it books for each take 31 MiB of the 128. The streams it had no room for sent 142 KB per frame, 20 frames a second: with `Refuse` they would have been answered 503 instead.

## Lowering the limit

`TestPressure` (`pressure.csv`): 500 board streams hold zstd 2^19 in a budget of 1 GiB: 867 MiB booked, 828 MiB of live heap. After `SetLimit(512 << 20)`, each moves to 2^18 with low memory at its next frame: 445 MiB booked, and 407 MiB of live heap after a collection. That frame starts without history and takes 10,925 bytes.

## What lith took from it

A hub makes a budget when it is given none: half the process's memory limit, for all that the streams hold, and no change at all without a memory limit. The budget books each stream's encoder, 32 KiB for the rest of the stream, and each view's newest frame. It gives each stream a tier at its first write, from the size of its largest write:

- The window must hold that write with a sixteenth to spare, so that a frame can refer to the one before. `MinWindow` raises it for pages whose frames refer back further than one write.
- A brotli stream takes the window that holds its frame and no larger one: the 235 KB board takes 140 bytes a frame at quality 4 and 2^18, and 141 at 2^19. It takes quality 4 while the budget stays at most half full, quality 3 after that, and a window below its frame (quality 3 at 2^16, 17,013 bytes a frame for that board) only when quality 3 at its window does not fit what is left. It goes out uncompressed only when even 2^16 does not fit.
- A zstd stream takes the largest tier within its fair share, since its bytes keep falling as its window grows: the 235 KB board takes 4,183 bytes a frame at 2^18 with low memory, 2,948 at 2^19 and 1,715 at 2^20; the 49 KB board 1,180 at 2^16 and 287 at 2^19. As streams open it moves down to the window that holds its frame and no further, takes 2^15 only when that window does not fit, and moves up again at a write once room frees up.
- A brotli stream keeps its tier until it closes. A stream with `live.Spec.Reconnect` whose window is below its frame, or that went out uncompressed, ends with `sse.Reconnect` once room frees up for a window that holds its frame, and the page opens it again: one stream a second, and a stream ID once a minute.

`TestBudgetCurve` (`curve.csv`) opens streams one by one on a hub with real encoders and frames: data tables of 49, 143 and 233 KB in one view that every stream watches, twice as many streams as the budget has MiB; and 150 tabs that each watch a 233 KB view of their own, in 150 MiB and in the least budget that holds them all at 2^18. After every ten opens every view changes and every stream writes, so that zstd streams move. The first uncompressed stream is the first whose first frame went out uncompressed or in raw blocks: a zstd stream sends raw blocks until the streams above their share have moved down. Booked is what the budget books once every stream is open, heap what the heap and the stacks hold after a collection; the test's streams have no net/http connections, for which the budget books part of its 32 KiB a stream. The bytes per frame are the mean of every stream's last frame, uncompressed ones included.

<!-- include: bench/budget/curve.md -->
| Frame | Views | Budget | Clients | Opened | Window holds the frame | Compressed | First uncompressed | Booked | Heap | Bytes per frame |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 49 KB | one | 64 MiB | brotli | 128 | 70 | 70 | 71st | 65.8 MiB | 60.7 MiB | 22,667 |
| 49 KB | one | 64 MiB | zstd | 128 | 90 | 90 | 38th | 64.5 MiB | 55.1 MiB | 15,312 |
| 49 KB | one | 64 MiB | half each | 128 | 81 | 81 | 52nd | 64.6 MiB | 57.0 MiB | 18,609 |
| 49 KB | one | 128 MiB | brotli | 256 | 139 | 139 | 140th | 131.3 MiB | 120.3 MiB | 22,937 |
| 49 KB | one | 128 MiB | zstd | 256 | 181 | 182 | 75th | 130.2 MiB | 111.5 MiB | 14,603 |
| 49 KB | one | 128 MiB | half each | 256 | 162 | 162 | 102nd | 129.7 MiB | 114.6 MiB | 18,480 |
| 49 KB | one | 150 MiB | brotli | 300 | 163 | 163 | 164th | 154.0 MiB | 141.2 MiB | 22,880 |
| 49 KB | one | 150 MiB | zstd | 300 | 213 | 213 | 88th | 152.5 MiB | 130.5 MiB | 14,555 |
| 49 KB | one | 150 MiB | half each | 300 | 188 | 190 | 120th | 152.2 MiB | 134.9 MiB | 18,421 |
| 143 KB | one | 64 MiB | brotli | 128 | 48 | 49 | 50th | 66.1 MiB | 61.7 MiB | 88,754 |
| 143 KB | one | 64 MiB | zstd | 128 | 71 | 72 | 38th | 65.7 MiB | 59.9 MiB | 63,012 |
| 143 KB | one | 64 MiB | half each | 128 | 57 | 60 | 44th | 65.4 MiB | 60.3 MiB | 76,574 |
| 143 KB | one | 128 MiB | brotli | 256 | 97 | 98 | 99th | 132.9 MiB | 124.1 MiB | 89,131 |
| 143 KB | one | 128 MiB | zstd | 256 | 143 | 143 | 75th | 131.0 MiB | 119.1 MiB | 64,748 |
| 143 KB | one | 128 MiB | half each | 256 | 117 | 119 | 87th | 131.5 MiB | 121.2 MiB | 77,787 |
| 143 KB | one | 150 MiB | brotli | 300 | 114 | 114 | 115th | 155.4 MiB | 145.0 MiB | 89,163 |
| 143 KB | one | 150 MiB | zstd | 300 | 168 | 168 | 88th | 153.9 MiB | 139.8 MiB | 63,252 |
| 143 KB | one | 150 MiB | half each | 300 | 139 | 139 | 102nd | 154.0 MiB | 142.0 MiB | 77,175 |
| 233 KB | one | 64 MiB | brotli | 128 | 48 | 49 | 50th | 66.2 MiB | 62.3 MiB | 144,803 |
| 233 KB | one | 64 MiB | zstd | 128 | 71 | 71 | 38th | 65.2 MiB | 60.2 MiB | 106,031 |
| 233 KB | one | 64 MiB | half each | 128 | 58 | 59 | 44th | 65.1 MiB | 60.5 MiB | 126,950 |
| 233 KB | one | 128 MiB | brotli | 256 | 97 | 97 | 98th | 132.3 MiB | 124.6 MiB | 146,277 |
| 233 KB | one | 128 MiB | zstd | 256 | 143 | 143 | 75th | 131.1 MiB | 120.6 MiB | 107,002 |
| 233 KB | one | 128 MiB | half each | 256 | 118 | 118 | 86th | 131.4 MiB | 122.4 MiB | 128,340 |
| 233 KB | one | 150 MiB | brotli | 300 | 114 | 114 | 115th | 155.5 MiB | 146.5 MiB | 145,428 |
| 233 KB | one | 150 MiB | zstd | 300 | 168 | 168 | 88th | 154.0 MiB | 141.7 MiB | 103,090 |
| 233 KB | one | 150 MiB | half each | 300 | 139 | 139 | 102nd | 154.1 MiB | 143.5 MiB | 125,854 |
| 233 KB | one each | 150 MiB | brotli | 150 | 97 | 97 | 98th | 163.7 MiB | 157.8 MiB | 84,310 |
| 233 KB | one each | 150 MiB | zstd | 150 | 133 | 134 | 77th | 154.0 MiB | 146.0 MiB | 28,092 |
| 233 KB | one each | 150 MiB | half each | 150 | 110 | 111 | 88th | 155.5 MiB | 148.4 MiB | 64,460 |
| 233 KB | one each | 240 MiB | brotli | 150 | 150 | 150 | none | 233.4 MiB | 226.1 MiB | 3,293 |
| 233 KB | one each | 170 MiB | zstd | 150 | 150 | 150 | 88th | 168.0 MiB | 159.0 MiB | 5,465 |
| 233 KB | one each | 260 MiB | half each | 150 | 150 | 150 | none | 259.4 MiB | 251.1 MiB | 3,191 |
<!-- end include -->

- 150 MiB holds 97 brotli tabs with a 233 KB view of their own at 2^18, 133 zstd tabs, and 110 when half are each. Every tab's frame is booked, the uncompressed ones' too: the brotli tabs book 163.7 MiB.
- All 150 tabs hold 2^18 in 240 MiB with brotli (233.4 MiB booked, 226.1 MiB of heap: 65 at quality 4, 85 at quality 3), in 170 MiB with zstd (168.0 and 159.0 MiB), and in 260 MiB half each, where the zstd tabs take 2^19 while there is room. With the default limit, half of GOMEMLIMIT, 150 brotli tabs need a GOMEMLIMIT of 480 MiB.
- With brotli and zstd tabs mixed, the zstd streams that opened first hold large tiers until a stream finds no room and they write again, and brotli streams that open meanwhile go out uncompressed. With `Reconnect`, those come back once the zstd streams have moved down.

[Streams and compression](/docs/guides/streams#the-memory-budget) shows the settings.

## Measuring again

The tests run only with `LITH_BUDGET=1`, and write the CSV files next to them:

```sh
LITH_BUDGET=1 LITH_BUDGET_RUNS=3 taskset -c 0-11 go test ./bench/budget -run Settings -v -timeout 1h
```

`-run` picks the test: `Settings`, `Floor`, `FrameSizes`, `WindowMargin`, `ViewCost`, `BudgetCurve`, `Simulation`, `Hibernation`, `WakeCost`, `Loadtest` or `Pressure`. After a run, `go generate ./content` in `site/` draws this page's charts from the new files.
