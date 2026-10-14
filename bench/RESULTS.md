# Benchmark results

## Writer

Measured on 2026-10-12 on an Intel i5-13500 (20 threads) under Linux 6.18 (Unraid), with Go 1.27.1 and modernc.org/sqlite v1.60.1 (SQLite 3.53.4). The database sits in `/tmp`, a btrfs image on the host's SSD cache pool. The runs are pinned to the performance cores, as kanban's were:

    taskset -c 0-11 go test ./writer -run '^$' -bench Writer -benchtime 5s -count 3

`BenchmarkWriter` (`writer/bench_test.go`) runs the writer alone, without HTTP: 1, 50 or 200 producers, each moving money between ten accounts of its own and waiting for the commit with `Do`, as a request handler does. A command reads a balance, debits one account, credits another and appends to a ledger, then touches a topic: four statements, plus the writer's `SAVEPOINT` and `RELEASE`. SQLite checkpoints the WAL itself (`CheckpointAuto`). Medians of three runs, the range in brackets:

| synchronous | producers | commands/s | commands per transaction | ms per transaction |
|---|---|---|---|---|
| FULL | 1 | 526 (511 to 579) | 1 | 1.9 |
| FULL | 50 | 13,260 (12,357 to 14,025) | 25 | 1.9 |
| FULL | 200 | 34,893 (27,317 to 37,044) | 122 | 3.5 |
| NORMAL | 1 | 13,049 (11,199 to 15,793) | 1 | 0.07 |
| NORMAL | 50 | 63,267 (62,423 to 79,296) | 26 | 0.39 |
| NORMAL | 200 | 66,688 (63,440 to 77,502) | 109 | 1.4 |

A transaction's time runs from `BEGIN IMMEDIATE` to the end of `COMMIT`. The host runs other services, and the NORMAL runs vary the most.

With FULL, a producer alone waits for one fsync per command, 1.9 ms on this disk. Fifty producers share each fsync, 25 commands at a time, and get 25 times as far; at 200 a transaction takes 122 commands.

With NORMAL the writer is bound by the CPU: about 13 µs per command at 200 producers. In a CPU profile of that case, `sqlite3_step` takes 42% of the process's samples, the rest of the writer's goroutine (database/sql, the driver's binding, locks) 32%, and the garbage collector 8%. The writer's own code (queue, savepoints, statement cache) is under 2%.

At 200 producers the slowest 1% of transactions take 7 to 26 ms, because SQLite checkpoints inside the `COMMIT` that brings the WAL past 1000 pages. `BenchmarkWriterCheckpoint` runs the same case with `CheckpointWriter`, where the writer checkpoints after it has answered the batch:

| synchronous | producers | commands/s | commands per transaction | ms per transaction | p99 ms per transaction |
|---|---|---|---|---|---|
| FULL | 200 | 45,100 (44,075 to 46,228) | 125 | 2.6 | 4.6 to 5.3 |
| NORMAL | 200 | 71,381 (68,629 to 74,217) | 102 | 1.1 | 3.0 to 3.8 |

Here the checkpoint is not part of a transaction's time; the producers queue their next commands while it runs.

kanban's `BenchmarkMoveCard` on the same machine (2026-10-08, FULL) moved 240 to 340, about 5,100 and about 13,800 cards a second with 1, 50 and 200 people, in transactions of 1, 25 and 103 moves. A move runs about fifteen statements, and a transaction alone took 2.9 to 4.2 ms there. Anders Murphy's benchmark reports 98,163 transactions a second with a savepoint each and FULL on an M1 MacBook, two updates per transaction; without `PRAGMA fullfsync`, macOS's fsync does not wait for the drive's cache, so NORMAL is the closer comparison here.

## On the site's server

Measured on 2026-10-14 between 05:08 and 05:12 UTC on the VPS that serves lith.zweiundeins.gmbh: four vCPUs of an AMD EPYC (Genoa) and 4 GB of memory for about 10 dollars a month, shared with other apps and with other tenants of its host, with Go 1.27.2 and modernc.org/sqlite v1.60.1, on lith's main at 7ee5af7. The test binaries ran at normal priority, not pinned, with the databases in `/var/tmp` on the VPS's disk. The 1-minute load average was 0.42 before the runs and 1.94 during them:

    go test -c -o writer.test ./writer && go test -c -o loadtest.test ./loadtest
    TMPDIR=/var/tmp ./writer.test -test.run '^$' -test.bench 'Writer$' -test.benchtime 5s -test.count 3
    TMPDIR=/var/tmp ./loadtest.test -test.run '^$' -test.bench 'SharedBoard/.*/zstd' -test.count 3

`BenchmarkWriter`, as on the development machine above. Medians of three runs, the range in brackets:

| synchronous | producers | commands/s | commands per transaction | ms per transaction |
|---|---|---|---|---|
| FULL | 1 | 143 (119 to 163) | 1 | 7.0 |
| FULL | 50 | 3,986 (3,879 to 4,103) | 25 | 6.3 |
| FULL | 200 | 14,480 (11,980 to 15,833) | 108 | 7.4 |
| NORMAL | 1 | 13,436 (13,271 to 13,539) | 1 | 0.07 |
| NORMAL | 50 | 64,138 (62,404 to 65,707) | 22 | 0.33 |
| NORMAL | 200 | 80,530 (73,684 to 81,096) | 108 | 1.3 |

With NORMAL the VPS commits more commands a second than the development machine; with FULL a transaction waits about 7 ms for the VPS's fsync, against 1.9 ms there, and two hundred producers get 14,480 commands a second instead of 34,893.

`BenchmarkSharedBoard` with zstd, 200 streams on one board while 200 producers send 800 commands a second:

| | FULL | NORMAL |
|---|---|---|
| CPU, cores busy | 0.54 (0.51 to 0.55) | 0.55 (0.53 to 0.59) |
| ticks a second | 16.4 (16.3 to 18.8) | 19.8 (19.3 to 19.8) |
| tick p95 | 3.0 ms (2.84 to 3.06) | 1.1 ms (1.09 to 1.40) |
| renders per tick | 1.00 | 1.00 |
| frames dropped a second | 0 | 0 (0 to 18.9) |
| bytes per frame on the wire | 2,194 (2,167 to 2,261) | 2,188 (2,156 to 2,265) |
| heap per stream | 1.66 MiB (1.66 to 1.68) | 1.68 MiB (1.66 to 1.68) |
| commands per transaction | 5.9 (5.5 to 6.1) | 1.02 (1.02 to 1.06) |
| command from due to commit, p50 | 10.5 ms (10.1 to 10.5) | 0.42 ms (0.40 to 0.43) |
| command from due to commit, p99 | 437 to 1,622 ms | 9 to 185 ms |

In one NORMAL run a tick took 252 ms, the VPS's neighbours at work, and the streams dropped 19 frames a second of that run: a stream that falls behind gets the newest frame, not every one.

## Read pool statement cache

Measured on 2026-10-13 between 20:50 and 20:52 UTC on the same machine, with Go 1.27.2 and modernc.org/sqlite v1.60.1 (SQLite 3.53.4), on lith's stmtcache branch at 6bbf9a3, pinned to the performance cores, which gives GOMAXPROCS 12. Six runs, each through every case once:

    taskset -c 0-11 go test ./sqlite -run '^$' -bench View -benchmem -benchtime 2s

`BenchmarkView` (`sqlite/bench_test.go`) reads a page in one `db.View`, as a GET handler does, with the read pool's statement cache (`sqlite.Options.Statements` at its default, 128) and without it (-1), from one goroutine and from twelve (`RunParallel`: the wall time divided by the pages of all twelve). The board is kanban's `ReadBoard` over kanban's seed board of 200 cards in 8 lists: eight queries, four of them joins, one with a `GROUP BY`, and the cards sorted. The guestbook is the lith-new template's page: a `count(*)` and the newest 20 of 1000 entries. Medians of the six runs, the range in brackets:

| µs per page | board, 1 goroutine | board, 12 goroutines | guestbook, 1 goroutine | guestbook, 12 goroutines |
|---|---|---|---|---|
| with the cache | 658 (646 to 689) | 397 (355 to 401) | 29.9 (26.4 to 33.0) | 7.8 (6.8 to 9.5) |
| without | 732 (723 to 788) | 410 (366 to 464) | 37.1 (32.8 to 42.5) | 8.5 (7.4 to 10.9) |
| less with the cache | 10% | 3% | 19% | 8% |

Every run read the serial pages faster with the cache, the parallel ones five runs of six. Both ways allocate the same: 1,646 allocations and 146 KB per board page, 216 and 10 KB per guestbook page.

The cache saves modernc's parse and plan. In CPU profiles of the serial cases (`-cpuprofile`, 3 s each), preparing took 11.2% of the board's samples without the cache and 0.9% with it, mostly the `ROLLBACK` that ends each View, which modernc runs through `sqlite3_exec`; the rest is running the queries and scanning 200 cards. The guestbook prepared in 26.6% of its samples without the cache and still in 18.0% with it: SQLite 3.53 reads a bound `LIMIT ?` when it prepares the statement and prepares it again whenever the parameter is bound, which modernc does at every run. A variant of the guestbook's read with `LIMIT 20` in its SQL took 21.6 µs with the cache and 29.8 µs without, 28% less, where the same read with `LIMIT ?` took 25.2 and 30.4 µs (medians of three runs).

Twelve goroutines gain less because they wait for modernc's memory allocator, whose malloc and free share one lock in the process: in a CPU profile of the cached board with twelve goroutines, 23% of the samples wait for it.

`TestStatementMemory` fills one connection's cache with 15 variants of the board's eight queries: 120 statements hold 490 KB by `DBSTATUS_STMT_USED`, 4.0 KiB a statement, so a full cache of 128 takes about 0.5 MiB per connection. The first statements on a connection count more, 7.0 KiB each for the board's first eight: they fill the connection's lookaside slots, which SQLite counts at their full size and allocates with the connection anyway.

A full `ANALYZE` takes the gain back from statements whose parameter meets an indexed column: modernc builds SQLite with `SQLITE_ENABLE_STAT4`, and once `sqlite_stat4` has samples, such a parameter steers the plan, so SQLite prepares the statement again at every run. After `ANALYZE` on the benchmark's database (136 rows in `sqlite_stat4`), the cached board read took 798 µs against 704 µs before, in one run each, with re-preparing at 19% of the samples. `PRAGMA optimize`, which the writer runs every six hours and on Close, wrote 13 rows to `sqlite_stat1` and none to `sqlite_stat4`, and the board kept its statements. Both cases hold for any prepared statement, the writer's and the render workers' as well.

The 1-minute load average was 3.37 when the runs began, rose to 5.54 during them, the benchmark's own load included, and was 3.81 after them. A first set with `-count 6`, which runs each case six times in a row, put a minute between a case with the cache and the same case without, while the load rose from 3.47 to 10.57; the drift hid the board's difference (799 against 807 µs), so the runs above go through every case once each.

## Render loop and streams, in process

Measured on 2026-10-13 on the same machine, an Intel i5-13500 (20 threads), with Go 1.26.5 and modernc.org/sqlite v1.60.1, the database in `/tmp` as above, on lith's main at 6265bce (with the render loop fixes) plus the loadtest branch. The runs are pinned to the performance cores, which gives GOMAXPROCS 12. Two sets of three runs, at 04:25 and 04:30 UTC:

    taskset -c 0-11 go test ./loadtest -run '^$' -bench SharedBoard -count 3

`BenchmarkSharedBoard` (`loadtest/bench_test.go`) is kanban's 200-tab run without HTTP: 200 streams on one board of 200 cards (`loadtest.Board`, 139 KB of HTML), while 200 producers send 800 commands a second between them. Each command adds one to a card's counter, so the board changes in every tick. The database runs with `synchronous=FULL` and the writer's checkpoints. Each stream compresses every frame and hands it to a client that takes it at once: no network, no load tool. Each run warms up for 2 s and is measured for 10 s. Medians of the six runs, the range in brackets:

| | zstd | brotli |
|---|---|---|
| CPU, cores busy | 1.59 (1.01 to 1.65) | 3.31 (3.00 to 3.61) |
| ticks a second | 19.8 (17.8 to 19.8) | 19.8 (19.7 to 19.9) |
| tick p50 | 2.5 ms (1.12 to 3.47) | 0.87 ms (0.75 to 1.69) |
| tick p95 | 4.6 ms (4.07 to 4.81) | 2.1 ms (1.65 to 2.52) |
| tick p99 | 5.2 ms (5.00 to 5.79) | 2.7 ms (2.10 to 3.24) |
| tick max | 6.5 ms (5.25 to 9.72) | 3.6 ms (2.31 to 5.83) |
| renders per tick | 1.00 | 1.00 |
| frames sent a second | 3,954 (3,542 to 3,960) | 3,966 (3,953 to 3,974) |
| frames dropped | 0 | 0 |
| bytes per frame on the wire | 2,205 (2,098 to 2,315) | 955 (934 to 1,000) |
| heap per stream | 1.67 MiB (1.66 to 1.68) | 2.13 MiB |
| commands a second | 800 | 800 |
| commands per transaction | 2.1 (2.09 to 3.61) | 2.0 (1.40 to 2.27) |
| command from due to commit, p50 | 5.1 ms (4.91 to 8.38) | 3.9 ms (2.85 to 4.86) |
| command from due to commit, p99 | 13 to 747 ms | 17 to 61 ms |

A tick renders the board once for all 200 streams and hands them the frame. No tick overran its 50 ms, and no stream missed a frame. In a CPU profile of one run per encoding (`-cpuprofile`, same command), rendering takes 0.3% of the samples with brotli and 3.6% with zstd, the writer 3.5% and 10.6%. kanban renders its board in 2.4 ms.

Compression is the cost: 91% of the samples with brotli, 74% with zstd. zstd sends 2.3 times the bytes of brotli per frame (2,205 against 955) for half the CPU (1.59 against 3.31 cores). kanban's load tool measured 999 bytes per frame with brotli at quality 4, and 4.9 cores for its server; the 3.3 cores here are the streams, the render loop and the writer, without HTTP. A quarter of zstd's samples are `memmove`, four fifths of them in the encoder's `addBlock`, which copies each frame into its history.

zstd's numbers depend on what else the machine does. Four zstd runs alternated with and without twelve `nice -n 19 yes > /dev/null` loops pinned to the same cores: with the loops, the process used 0.80 and 0.86 cores at a tick p50 of 1.25 ms; without them, 1.50 and 1.90 cores at 1.7 and 4.1 ms, and in CPU profiles of such a pair the same compression took 12.7 s against 8.0 s. The zstd process leaves most of its cores idle between ticks, and cores that keep falling idle do the same work in more time: through the CPU's power management (intel_pstate, powersave) or through the scheduler moving threads to idle cores, which these runs do not tell apart. brotli keeps three cores busy itself and varies less. Compare runs made under the same conditions.

The heap per stream is what the sse package measures: 1.67 MiB with zstd's full history, 2.13 MiB with brotli at quality 4 and a 2^18 window. 200 tabs hold 334 MiB or 426 MiB.

The writer kept up with 800 commands a second in every run, about two to a transaction. A command's time from when it was due to its commit is mostly the fsync: its p50 was 3 to 8 ms. Its p99 depends on the disk, the host's shared SSD pool: in two zstd runs of the second set, while another agent's test suites ran, the writer waited up to 0.8 s for the disk and then took up to 512 commands (MaxBatch) into one transaction; fewer commits made fewer ticks (17.8 a second in the slowest run).

Other agents compiled and ran test suites on the machine during both sets: the 1-minute load average was 4.7 before the first set and 9.7 after it, 10.3 before the second and 9.1 after it, on 20 threads.

### synchronous FULL against NORMAL

Measured on 2026-10-13 between 12:58 and 13:03 UTC on the same machine, with Go 1.27.2, modernc.org/sqlite v1.60.1 and klauspost/compress v1.20.3, on lith's main at 8db2b69 plus the change that makes NORMAL the default. `BenchmarkSharedBoard` now runs each encoding with `synchronous=FULL`, then NORMAL. Two sets of three runs:

    taskset -c 0-11 go test ./loadtest -run '^$' -bench SharedBoard -count 3

Medians of the six runs, the range in brackets:

| | FULL, zstd | NORMAL, zstd | FULL, brotli | NORMAL, brotli |
|---|---|---|---|---|
| CPU, cores busy | 0.98 (0.66 to 1.82) | 1.04 (0.77 to 1.62) | 2.53 (1.89 to 3.28) | 2.80 (2.51 to 3.07) |
| ticks a second | 19.6 (9.4 to 19.8) | 19.4 (17.3 to 19.7) | 15.8 (13.1 to 19.2) | 19.8 (18.8 to 19.8) |
| tick p95 | 4.0 ms (1.19 to 4.72) | 4.2 ms (1.16 to 4.58) | 1.4 ms (1.14 to 2.92) | 2.0 ms (1.14 to 2.76) |
| frames dropped | 0 | 0 | 0 | 0 |
| commands per transaction | 1.78 (1.29 to 7.32) | 1.07 (1.04 to 1.19) | 3.63 (1.81 to 4.37) | 1.03 (1.03 to 1.09) |
| command from due to commit, p50 | 3.4 ms (2.7 to 323) | 0.54 ms (0.52 to 0.61) | 11.0 ms (3.1 to 11.8) | 0.30 ms (0.28 to 0.32) |
| command from due to commit, p99 | 154 ms (21 to 2,715) | 172 ms (62 to 808) | 777 ms (146 to 1,945) | 34 ms (23 to 448) |

With NORMAL a command commits in about half a millisecond at p50; with FULL it waits for its batch's fsync, 3 to 11 ms, and the writer takes two to four commands into a transaction instead of one. The render loop does the same work in both modes: one render a tick, no dropped frames, and the CPU goes to the encoders. With FULL and brotli the slower commits leave some ticks with nothing new, 15.8 ticks a second against 19.8.

The p99s compare poorly: they are the shared SSD pool's. A run with a disk stall waits for seconds in either mode (FULL with zstd once 2.7 s, at 9.4 ticks a second). The 1-minute load average was 1.5 before the first set and 5.7 after the second, the benchmark's own work included.

What NORMAL saves is latency, and what it costs is durability: a power loss or kernel crash can undo up to about 35 s of acknowledged commits, those since the writer's last checkpoint, with Linux's default writeback. A crash of the process loses none.

### zstd levels

Measured on 2026-10-13 between 05:02 and 05:04 UTC on the same machine and toolchain, with klauspost/compress v1.20.1, on lith's api-core branch, which adds `sse.Options.ZstdLevel`:

    taskset -c 0-11 go test ./loadtest -run '^$' -bench BoardZstdLevels -count 3

`BenchmarkBoardZstdLevels` (`loadtest/bench_test.go`) is `BenchmarkSharedBoard` with every stream on zstd, at SpeedFastest (lith's default), SpeedDefault and SpeedBetterCompression. Medians of three runs each, the range in brackets:

| | fastest | default | better |
|---|---|---|---|
| bytes per frame on the wire | 2,266 (2,183 to 2,283) | 1,138 (1,087 to 1,153) | 365 (364 to 366) |
| CPU, cores busy | 1.32 (1.05 to 1.41) | 1.07 (1.07 to 1.15) | 3.11 (3.08 to 3.20) |
| heap per stream | 1.68 MiB | 2.76 MiB | 5.56 MiB |
| tick p99 | 5.2 ms (2.80 to 5.24) | 2.5 ms (2.03 to 4.38) | 2.8 ms (1.97 to 3.99) |

Every run sent 3,920 to 3,969 frames a second and dropped none. The default level halves the bytes of the fastest for no more CPU in these runs, and costs 1.1 MiB more per stream: about 220 MiB more for 200 tabs. Better compression sends a third of the default's bytes and fewer than brotli's 955 (above), for about brotli's CPU (3.31 cores) and two and a half times its heap. The 1-minute load average was 6.8 before the runs and 11.9 after them, from other agents' work, and the fastest level varied the most, as in the runs above.

`TestHeapPerStream` (`sse/heap_test.go`) measures the same levels on one core, without the render loop: 200 frames of a 57 KB page, of which five rows change from one frame to the next, to each of 20 streams. Three runs of

    go test ./sse -run TestHeapPerStream -v -count 3

gave the same bytes in each run:

| | bytes per frame | time per frame | heap per stream |
|---|---|---|---|
| zstd fastest | 405 | 36 to 41 µs | 1.65 MiB |
| zstd fastest, `ZstdLowMemory` | 405 | 55 to 58 µs | 1.02 MiB |
| zstd default | 228 | 31 µs | 2.73 MiB |
| zstd better | 108 | 184 to 201 µs | 5.54 MiB |
| brotli, quality 4 | 121 | 251 to 265 µs | 2.02 MiB |

The `memmove` in zstd's `addBlock` comes from its history: the encoder keeps twice its window (1 MiB for the 512 KiB window) and, when the next block does not fit, moves the newest 512 KiB to the front. With a 140 KB frame that is one 512 KiB move every three or four frames, as many bytes again as copying each frame in. No encoder option changes the ratio: a larger window moves more bytes less often, and `ZstdLowMemory` moves once for every frame.

## Stream budget and hibernation

Measured on 2026-10-13 between 15:20 and 16:10 UTC on the same machine, with Go 1.27.2, klauspost/compress v1.20.3 and andybalholm/brotli v1.2.6, on lith's budget branch, pinned to the performance cores. The tests in `bench/budget` run only with `LITH_BUDGET=1` and write the CSV files next to them:

    LITH_BUDGET=1 LITH_BUDGET_RUNS=3 taskset -c 0-11 go test ./bench/budget -run Settings -v -timeout 1h

Other agents' work kept the 1-minute load average between 3.3 and 5.6 while the CPUs stayed about 90% idle; during the load tests it rose to 6.6, with the test's own process at up to 4.5 cores.

### Settings

Two workloads. The board is `loadtest.Board`'s view rendered from SQLite: 200 frames of 142 KB, one card changed before each. The page is a synthetic tab of a small business app shaped like libretto's: lists of up to 72 entries, a form in a modal, a detail page and an overview, 150 frames of 2.4 to 36.9 KB (13.6 KB on average), most of them a view the tab saw before with a change. `TestSettings` writes the frames to 64 streams in turn, as the hub does, and measures the heap the open streams keep after two collections, divided by 64, the bytes per frame on the wire after the first frame, and the process's CPU time per frame, the median of three runs (the runs differ by under 6%). zstd runs at its fastest level (`settings.csv`):

| Setting | Board: heap | bytes per frame | CPU per frame | Page: heap | bytes per frame | CPU per frame |
|---|---|---|---|---|---|---|
| zstd 2^15 | 1.62 MiB | 11,234 | 161 µs | 1.62 MiB | 1,130 | 21.6 µs |
| zstd 2^15, low memory | 0.53 MiB | 11,234 | 159 µs | 0.52 MiB | 1,130 | 20.3 µs |
| zstd 2^16 | 1.66 MiB | 10,872 | 160 µs | 1.65 MiB | 571 | 14.6 µs |
| zstd 2^16, low memory | 0.63 MiB | 10,872 | 156 µs | 0.58 MiB | 571 | 13.2 µs |
| zstd 2^17 | 1.66 MiB | 10,831 | 167 µs | 1.65 MiB | 379 | 12.4 µs |
| zstd 2^17, low memory | 0.69 MiB | 10,831 | 167 µs | 0.64 MiB | 379 | 11.3 µs |
| zstd 2^18 | 1.66 MiB | 2,703 | 68.3 µs | 1.65 MiB | 248 | 10.3 µs |
| zstd 2^18, low memory | 0.81 MiB | 2,703 | 76.8 µs | 0.76 MiB | 248 | 10.3 µs |
| zstd 2^19 | 1.66 MiB | 1,471 | 62.3 µs | 1.65 MiB | 189 | 9.5 µs |
| zstd 2^19, low memory | 1.06 MiB | 1,471 | 84.1 µs | 1.01 MiB | 189 | 10.8 µs |
| zstd 2^20 | 2.66 MiB | 794 | 55.6 µs | 2.65 MiB | 163 | 10.0 µs |
| zstd 2^20, low memory | 1.56 MiB | 794 | 124 µs | 1.51 MiB | 163 | 12.0 µs |
| brotli q3 2^16 | 0.89 MiB | 9,999 | 709 µs | 0.69 MiB | 311 | 50.0 µs |
| brotli q3 2^17 | 1.02 MiB | 10,006 | 693 µs | 0.82 MiB | 242 | 48.4 µs |
| brotli q3 2^18 | 1.27 MiB | 1,823 | 349 µs | 1.07 MiB | 186 | 46.0 µs |
| brotli q3 2^19 | 1.77 MiB | 899 | 309 µs | 1.57 MiB | 161 | 45.7 µs |
| brotli q3 2^20 | 2.77 MiB | 411 | 294 µs | 2.57 MiB | 157 | 47.1 µs |
| brotli q4 2^16 | 1.75 MiB | 9,716 | 889 µs | 1.22 MiB | 239 | 60.8 µs |
| brotli q4 2^17 | 1.88 MiB | 9,648 | 879 µs | 1.34 MiB | 198 | 57.9 µs |
| brotli q4 2^18 | 2.13 MiB | 986 | 374 µs | 1.59 MiB | 161 | 56.3 µs |
| brotli q4 2^19 | 2.63 MiB | 389 | 347 µs | 2.09 MiB | 147 | 55.8 µs |
| brotli q4 2^20 | 3.63 MiB | 161 | 334 µs | 3.09 MiB | 143 | 57.0 µs |
| gzip | 0.80 MiB | 13,169 | 150 µs | 0.79 MiB | 1,318 | 16.7 µs |
| identity | 1 KiB | 142,067 | 0.2 µs | 1 KiB | 13,703 | 0.3 µs |

- A window that cannot reach the frame before costs the board four times the bytes and over twice the CPU: below 2^18 a frame finds the previous one out of reach. The page loses less, since a window still holds several of its frames: against 2^19, it sends twice the bytes at 2^17, three times at 2^16 and six times at 2^15. libretto's real frames lose less (9% more at 2^17, 19% at 2^16): their measured session repeats a few views more often than this page.
- zstd with its full history holds 1.6 MiB at every window up to 2^19, so below 2^19 only low memory saves heap. It costs CPU with the board's frames (84 µs against 62 at 2^19, 124 against 56 at 2^20) and 1 to 2 µs with the page's.
- On the board, brotli at quality 3 holds 0.86 MiB less than quality 4 at the same window and sends about twice its bytes. Both take four to six times zstd's CPU.

`TestFloor` (`floor.csv`, the page) finds what a compressed stream holds whatever its window: zstd with low memory holds 0.44 MiB at its smallest window, 2^10 (its 256 KiB hash table, 129 KiB of history and its block buffers), brotli at quality 2 or 3 0.60 MiB at windows up to 2^14, gzip 0.79 MiB. Below 0.44 MiB a stream only goes out uncompressed.

### Hibernation

`TestHibernation` (`hibernate.csv`) simulates an hour of 200 tabs, second by second, with the page's frames: each person changes something every three minutes on average (a modal, then the list it changed a second later), and the hub sends a heartbeat after 20 s of silence. A tab that sent no frame for the hibernation time ends its zstd frame and gives its encoder back. The heap per tab is the mean of samples every five minutes after the first ten, after a collection, which also empties the pool of encoders.

| Setting | Hibernate after | Heap per tab | Tabs awake | Bytes per frame | Heartbeats per tab and hour |
|---|---|---|---|---|---|
| zstd 2^17, low memory | never | 0.640 MiB | 100% | 433 | 1,010 B |
| | 30 s | 0.106 MiB | 16% | 1,533 | 2,412 B |
| | 120 s | 0.308 MiB | 48% | 1,103 | 1,798 B |
| zstd 2^19 | never | 1.647 MiB | 100% | 277 | 1,010 B |
| | 30 s | 0.268 MiB | 16% | 1,532 | 2,412 B |
| | 120 s | 0.787 MiB | 48% | 1,093 | 1,798 B |

After 30 s, a sixth of the encoders stay and a frame takes about 1.1 KB more: 41% of the frames start a zstd frame without history (1,947 bytes on average), and the list after a modal finds only the modal in its frame. That is about 45 KB more per tab and hour. A heartbeat of a hibernating stream goes out in a frame of its own, 15 bytes instead of 6. Waking takes 36 to 62 µs for the page's 37 KB list with an encoder from the pool and 101 to 117 µs with a new one, 124 µs and 303 to 467 µs for the board's frame (`TestWakeCost`); the list then takes 3,790 bytes on the wire, the board 10,930.

Headless Chromium 154 (`TestChromiumDecodesFrames` in `sse`) decodes a zstd body of several frames event by event, each as soon as it is sent: frames that hibernation ended, raw frames while a budget had no room, frames from pooled encoders. Given a brotli body that holds a second brotli stream after the first, it decodes the first and drops the second without an error.

### A budget as streams grow

`TestSimulation` (`simulation.csv`) opens up to 2,000 streams of one workload and, after every ten, adds up the measured heap and bytes per frame of what each stream holds. The budgets are `sse.Budget` with MinWindow at the workload's floor (2^18 for the board, 2^17 for the page) and MaxWindow 2^19. "At open only" opens the streams; in the others every open stream writes after every ten opens, so that tiers shrink and waiting streams take the room that frees up. At 2,000 streams:

| Policy | Board: compressed | heap | bytes per frame | Page: compressed | heap | bytes per frame |
|---|---|---|---|---|---|---|
| lith's default, zstd 2^19 | 2,000 | 3,312 MiB | 1,471 | 2,000 | 3,294 MiB | 189 |
| zstd at the floor, low memory | 2,000 | 1,626 MiB | 2,703 | 2,000 | 1,278 MiB | 379 |
| budget of 128 MiB, at open only | 77 | 129 MiB | 136,654 | 77 | 129 MiB | 13,183 |
| budget of 128 MiB | 154 | 127 MiB | 131,336 | 182 | 118 MiB | 12,491 |
| budget of 128 MiB, 503 | 154, 1,846 refused | 125 MiB | 2,703 | 182, 1,818 refused | 116 MiB | 379 |
| budget of 1 GiB | 1,236 | 1,006 MiB | 55,940 | 1,456 | 931 MiB | 4,003 |

At open only, the first 77 streams take zstd 2^19 and leave no room for the rest. When streams that find no room make the others shrink, the budget holds as many streams as fit at the floor. A budget of 1 GiB gives the board's streams zstd 2^19 up to about 600 streams, 2^19 with low memory up to about 950, 2^18 with low memory up to 1,236, and nothing beyond.

### The board with and without a budget

`TestLoadtest` (`loadtest.csv`) runs `BenchmarkSharedBoard`'s board in process, while 200 producers change cards 800 times a second, about 40 cards per frame. Two runs each:

| Streams | Policy | Cores busy | Bytes per frame | Heap | Frames dropped |
|---|---|---|---|---|---|
| 200 | lith's default | 1.55, 1.63 | 2,177, 2,239 | 334 MiB | 0 |
| 200 | zstd 2^18, low memory | 1.81, 1.89 | 3,984, 3,881 | 164 MiB | 0 |
| 200 | budget of 128 MiB: 154 at zstd 2^18 low memory, 46 raw | 1.48, 1.47 | 35,718, 35,864 | 126 MiB | 0 |
| 1,000 | lith's default | 4.06, 4.13 | 2,226, 2,171 | 1,660 MiB | 0 |
| 1,000 | zstd 2^18, low memory | 4.52, 4.38 | 4,040, 3,996 | 816 MiB | 0 |
| 1,000 | budget of 128 MiB: 154 compressed, 846 raw | 2.11, 1.95 | 121,272, 121,288 | 129 MiB | 0 |

Every run held about 19.8 ticks a second. The streams the budget has no room for send 142 KB per frame, 20 frames a second: with `Refuse` they would be answered 503 instead.

### Lowering the limit

`TestPressure` (`pressure.csv`): 500 board streams hold zstd 2^19 in a budget of 1 GiB, 828 MiB. After `SetLimit(512 << 20)` each moves to 2^18 with low memory at its next frame: 414 MiB reserved, and 407 MiB of live heap after a collection, from 828. That frame starts without history and takes 10,925 bytes.

### What lith adopted

Since 2026-10-13 a live hub makes a budget when it is given none: half the process's memory limit, windows from 2^19 down to 2^18, uncompressed streams when even 2^18 does not fit, and no change at all without a memory limit (a stream's own ZstdWindow and BrotliWindow cap what it gets). Pages with large frames pass a budget with `Refuse`. Hibernation is opt-in: `live.Options.Hibernate` is zero unless an app sets it.
