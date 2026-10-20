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

## Large updates and deletes

Measured on 2026-10-19 between 11:37 and 11:42 UTC on the development machine of the Writer section, an Intel i5-13500 under Linux 6.18 (Unraid), with Go 1.27.2 and modernc.org/sqlite v1.60.1 (SQLite 3.53.4, FTS5 built in), on lith's main at 9819ecb (v0.3.1) with the benchmark added. The database sits in `/tmp` as above. Other agents' jobs shared the machine: the 1-minute load average was 11.4 when the runs began and 6.0 after them. The output of every run is in `bench/large-writes/bench.txt`:

    touch /tmp/lith-bench.lock
    flock /tmp/lith-bench.lock taskset -c 0-11 go test ./loadtest -run '^$' -bench LargeWrite -benchtime 1x -count 3 -timeout 1h

`BenchmarkLargeWrite` (`loadtest/largewrite_test.go`) runs one large write while the board of `BenchmarkSharedBoard` is under load: 200 zstd streams on one board of 200 cards, and 200 producers that change a card 800 times a second between them, each command with `Do`, so the board renders in every tick. The write changes a table of 45,000 comments, each about 2 KB of text and 2 KB of its HTML, 185 MB with the board. It either clears every comment's HTML (`UPDATE ... SET html = NULL`, as a one-time re-render does), deletes every row that a full pass did not see (`DELETE ... WHERE seen < ?`, as DeleteMissing does; without a `WHERE`, SQLite would truncate the table), or rebuilds the comments' FTS5 index (`INSERT INTO fts (fts) VALUES ('rebuild')`). It runs as one command; as commands of 1,000 or 5,000 rows by rowid range (`WHERE id BETWEEN ? AND ?`), sent one after the other with `Do`; or in `Writer.Exclusive`, in one transaction. The database runs with `CheckpointWriter`. Each run loads for 2 s, measures 2 s before the write, the write, and 2 s after it.

"Do while it ran" is the producers' commands due between the write's start and its last commit, from when each was due to its commit. Ticks, renders and frames count from the write's start to 2 s after its end; the longest gap is the longest time between two ticks that rendered, which is how long the pages saw no new frame. Medians of three runs, the range in brackets:

| synchronous | write | as | write took | Do while it ran, p50 | p99 | max | longest gap between ticks | tick p99 | WAL at most | longest checkpoint |
|---|---|---|---|---|---|---|---|---|---|---|
| NORMAL | update | one command | 0.49 s (491 to 499 ms) | 632 ms | 872 ms | 877 ms | 848 ms (473 to 866) | 0.82 ms | 186 MB | 383 ms (360 to 403) |
| NORMAL | update | chunks of 1,000 | 1.07 s (1.065 to 1.182) | 22.7 ms | 38.3 ms | 40.7 ms | 51 ms | 0.85 ms | 8.3 MB | 22 ms (19 to 24) |
| NORMAL | update | chunks of 5,000 | 0.89 s (0.842 to 0.899) | 100.3 ms | 156.4 ms | 163.0 ms | 153 ms (143 to 154) | 0.92 ms | 21.4 MB (21.3 to 41.2) | 62 ms (59 to 103) |
| NORMAL | update | `Exclusive` | 0.34 s (334 to 355 ms) | 530 ms | 704 ms | 707 ms | 693 ms (679 to 704) | 0.83 ms | 186 MB | 349 ms (348 to 374) |
| NORMAL | delete | one command | 0.10 s (102 ms) | 50.7 ms | 100.6 ms | 101.9 ms | 90 ms (87 to 94) | 1.09 ms | 4.1 MB | 31 ms (11 to 33) |
| NORMAL | delete | chunks of 1,000 | 0.12 s (112 to 149 ms) | 1.68 ms | 4.24 ms | 4.67 ms | 51 ms | 1.21 ms | 4.1 MB (4.1 to 5.4) | 16 ms (11 to 27) |
| NORMAL | delete | chunks of 5,000 | 0.10 s (101 to 103 ms) | 6.03 ms | 15.78 ms | 17.03 ms | 51 ms | 1.06 ms | 4.1 MB (4.1 to 8.2) | 11 ms (10 to 13) |
| NORMAL | delete | `Exclusive` | 0.11 s (95 to 118 ms) | 56.0 ms | 111.0 ms | 112.3 ms | 82 ms (64 to 108) | 2.15 ms | 8.2 MB (4.1 to 8.2) | 13 ms (10 to 22) |
| FULL | update | chunks of 1,000 | 1.03 s (0.994 to 1.163) | 26.3 ms | 42.2 ms | 45.2 ms | 53 ms | 4.42 ms | 8.3 MB | 15 ms (14 to 25) |
| FULL | update | chunks of 5,000 | 0.86 s (0.847 to 0.874) | 110.3 ms | 171.9 ms | 180.6 ms | 103 ms (101 to 104) | 3.92 ms | 23.3 MB (22.8 to 23.7) | 37 ms (35 to 37) |
| NORMAL | FTS5 rebuild | one command | 2.77 s (2.75 to 2.79) | 1,526 ms | 2,887 ms | 2,916 ms | 2,888 ms (2,872 to 2,891) | 3.72 ms | 56.3 MB (56.1 to 56.3) | 128 ms (122 to 142) |

In the 2 s before the write, the producers' commands took 1.1 to 4.3 ms at p99 with NORMAL and 11 to 41 ms with FULL; a command that was due just before the write and committed after it counts there too, so the maximum before is not a baseline. No run dropped a frame, and every tick rendered the board once.

A write in one command holds every other command for its own time and then for the checkpoint after it: the writer checkpoints between batches, and a passive checkpoint of the 186 MB that the update wrote took 360 to 403 ms. That is why the slowest command waited 877 ms for a write of 491 ms, and why commands due in the 2 s after the write still took up to 363 to 406 ms. The pages froze for as long: the hub went on ticking, its ticks as short as before, but with no commit there was nothing to render, and no frame went out for up to 866 ms. The WAL grew to the size of the change and stayed there; the writer truncates it only after a second without commands, which a busy app does not have.

In commands of 1,000 rows, the same update took twice as long, 1.07 s, and no other command waited longer than 45 ms: each chunk took about 24 ms, a passive checkpoint followed each, the WAL stayed at 8.3 MB, and the board rendered in every tick, never more than 51 ms apart. Chunks of 5,000 took 0.89 s, held commands up to 163 ms, left gaps of about 150 ms between frames and let the WAL grow to 21 to 41 MB.

`Exclusive` ran the update in 0.34 s against 0.49 s as a command, and held other commands and the pages as long as one command does, for the same checkpoint after it. A command runs in a savepoint inside the batch's transaction, and `Exclusive` does not; that is the likely difference, not measured apart.

The delete was cheap: 102 ms for all 45,000 rows in one command, and the WAL did not grow past the 4.1 MB it had before. Chunks of 5,000 held other commands at most 17 ms.

The FTS5 rebuild is one statement that reads every comment and writes a new index: 2.77 s, during which every command waited and the pages got no frame for 2.9 s. FTS5's `'rebuild'` has no range, so it cannot be split as it is; the index could be filled in chunks of rowids instead, which this benchmark does not measure.

With FULL, the chunked update took as long as with NORMAL, and its commands waited a few milliseconds more, each batch's fsync on top of the chunk.

## Events through the writer

Measured on 2026-10-16 between 04:34 and 04:39 UTC on the development machine of the Writer section, an Intel i5-13500 under Linux 6.18 (Unraid), with Go 1.27.2 and modernc.org/sqlite v1.60.1, on lith's `events` branch. Other jobs shared the machine (load average 1.8 to 3.0), so `BenchmarkWriter` ran again right after, for numbers of the same hour:

    nice -n 19 taskset -c 0-11 go test ./events -run '^$' -bench Execute -benchtime 5s -count 3
    nice -n 19 taskset -c 0-11 go test ./writer -run '^$' -bench 'Writer$' -benchtime 5s -count 3

`BenchmarkExecute` (`events/bench_test.go`) is `BenchmarkWriter` with events: 1, 50 or 200 producers, each renaming ten cards of its own with `events.Execute` and waiting for the commit with `Do`. A rename reads the card's state row, decides, appends one event, saves the row and runs one inline projector, which updates `card_titles`: five statements against the transfer's four, writing six B-trees (the event's row, its two indexes and `sqlite_sequence`, the state row, the projection) against the transfer's two. Medians of three runs, the range in brackets; `BenchmarkWriter` the same hour beside them:

| synchronous | producers | events/s | commands per transaction | ms per transaction | `BenchmarkWriter` commands/s |
|---|---|---|---|---|---|
| FULL | 1 | 241 (241 to 278) | 1 | 4.1 | 340 |
| FULL | 50 | 4,564 (4,299 to 5,216) | 25 | 5.5 | 6,650 |
| FULL | 200 | 13,769 (13,067 to 14,059) | 122 | 9.0 | 32,085 |
| NORMAL | 1 | 6,675 (6,670 to 6,969) | 1 | 0.15 | 24,404 |
| NORMAL | 50 | 17,251 (16,724 to 17,354) | 26 | 1.5 | 92,907 |
| NORMAL | 200 | 18,342 (18,105 to 18,875) | 120 | 6.5 | 81,087 |

With NORMAL an event costs about four times a transfer. In a CPU profile of 200 producers with NORMAL, `events.Execute` takes 55% of the samples, and the commit's WAL writes and SQLite's checkpoints 26%. The renames of 2,000 cards land on pages spread over the event index and the state rows, so a batch writes many more pages to the WAL than a batch of transfers, whose 2,000 accounts fit in a few dozen pages. With one producer the commit takes 51% of the samples. Without the projector, 200 producers with NORMAL appended 17,733 and 19,465 events a second in two runs of 3 s.

### A rebuild under readers

Measured on 2026-10-16 around 05:10 UTC on the same machine, `nice -n 19 taskset -c 0-11`, three runs:

    go test ./events -run '^$' -bench RebuildReaders -benchtime 1x -count 3

`BenchmarkRebuildReaders` (`events/rebuild_bench_test.go`) gives `card_titles` a new version over 100,000 events (20,000 cards, each created and renamed four times) and lets the Runner rebuild it in its default batches of 500, 200 commands, while four readers look up cards by id on the read pool's prepared statements. Every batch renames the tables, a schema change, so each statement a reader has prepared is prepared again at its first use after each batch commits.

| | run 1 | run 2 | run 3 |
|---|---|---|---|
| the rebuild | 0.92 s | 1.03 s | 0.93 s |
| a lookup, readers alone | 4.4 µs | 4.7 µs | 4.6 µs |
| a lookup during the rebuild | 4.1 µs | 4.5 µs | 4.2 µs |
| preparing the lookup again after a schema change | 53 µs | 80 µs | 53 µs |

The re-prepares do not show in the mean: a connection prepares each statement it uses once per batch, 200 times in the rebuild, about 11 ms of 53 µs each per statement over a second of rebuild, against some 250,000 lookups per reader in that second. A render worker that runs twenty statements and renders after every batch pays that twenty times, about 0.2 s of CPU for a rebuild of 100,000 events.

### Looking back to a position

Measured on 2026-10-16 around 05:15 UTC on the same machine, `nice -n 19 taskset -c 0-11`, load average 3.2 to 4.2:

    go test ./events -run '^$' -bench AsOf -count 3

`BenchmarkAsOf` (`events/asof_test.go`) reads 100 cards as of a random position of a log of 10,000 events (each card created and renamed 99 times), in one read transaction per look, as a render does. Medians of three runs:

| how | per look |
|---|---|
| the cards' titles from `title_history`, rows with `valid_from` and `valid_to` positions, one query | 0.46 ms |
| the cards' titles from a replay of the log up to the position (`SinceUntil`, `Decode`, the fold) | 12.6 ms |
| one card's state from its own stream (`StateAt`, about 50 events) | 0.12 ms |

The history table has no index besides its key (`card_id`, `valid_from`), so the query reads all its 10,000 rows: an index on `valid_from` alone made it 2.2 ms, with a lookup in the table per row before the position, and a covering index on (`valid_from`, `valid_to`, `card_id`, `title`) 0.35 ms. The replay decodes and folds half the log on average, 2.5 µs an event.

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

Measured on 2026-10-14 on the same machine, an Intel i5-13500 (20 threads), with Go 1.27.2, modernc.org/sqlite v1.60.1, klauspost/compress v1.20.3 and andybalholm/brotli v1.2.6, the database in `/tmp` as above, on lith's main at ff7e536 (v0.2.0). The runs are pinned to the performance cores, which gives GOMAXPROCS 12. Two sets of three runs, at 17:21 and 17:26 UTC:

    taskset -c 0-11 go test ./loadtest -run '^$' -bench SharedBoard -count 3

`BenchmarkSharedBoard` (`loadtest/bench_test.go`) is kanban's 200-tab run without HTTP: 200 streams on one board of 200 cards (`loadtest.Board`, 139 KB of HTML), while 200 producers send 800 commands a second between them. Each command adds one to a card's counter, so the board changes in every tick. The database runs with the writer's checkpoints, with `synchronous=FULL` and then NORMAL (below). Each stream compresses every frame and hands it to a client that takes it at once: no network, no load tool. Each run warms up for 2 s and is measured for 10 s. With FULL, medians of the six runs, the range in brackets:

| | zstd | brotli |
|---|---|---|
| CPU, cores busy | 1.82 (1.28 to 1.90) | 3.42 (3.38 to 3.58) |
| ticks a second | 19.8 | 19.9 (19.8 to 19.9) |
| tick p50 | 2.9 ms (0.77 to 3.30) | 1.2 ms (1.04 to 1.37) |
| tick p95 | 3.6 ms (3.48 to 3.89) | 1.8 ms (1.75 to 1.88) |
| tick p99 | 4.1 ms (3.76 to 4.31) | 2.1 ms (2.10 to 2.23) |
| tick max | 4.4 ms (4.13 to 5.11) | 2.3 ms (2.20 to 3.74) |
| renders per tick | 1.00 | 1.00 |
| frames sent a second | 3,960 (3,956 to 3,965) | 3,968 (3,964 to 3,977) |
| frames dropped | 0 | 0 |
| bytes per frame on the wire | 2,217 (2,153 to 2,297) | 854 (852 to 862) |
| heap per stream | 1.68 MiB (1.66 to 1.68) | 1.56 MiB |
| commands a second | 800 | 800 |
| commands per transaction | 2.4 (1.69 to 2.64) | 1.3 (1.25 to 1.40) |
| command from due to commit, p50 | 5.5 ms (3.48 to 5.86) | 2.7 ms (2.55 to 2.87) |
| command from due to commit, p99 | 9.5 to 13.1 ms | 6.4 to 7.8 ms |

A tick renders the board once for all 200 streams and hands them the frame. No tick overran its 50 ms, and no stream missed a frame. In a CPU profile of one more run per encoding with FULL (`-cpuprofile`, the samples under each package by `go tool pprof -focus`), rendering takes 0.6% of the samples with brotli and 3.8% with zstd, the writer 3.4% and 11.6%. kanban renders its board in 2.4 ms.

Compression is the cost: 94% of the samples with brotli, 76% with zstd. zstd sends 2.6 times the bytes of brotli per frame (2,217 against 854) for about half the CPU (1.82 against 3.42 cores). kanban's load tool measured 999 bytes per frame with brotli at quality 4, and 4.9 cores for its server; the 3.4 cores here are the streams, the render loop and the writer, without HTTP. A fifth of zstd's samples are `memmove`, three quarters of them in the encoder's `addBlock`, which copies each frame into its history.

zstd's numbers depend on what else the machine does. On 2026-10-13, on lith's main at 6265bce, four zstd runs alternated with and without twelve `nice -n 19 yes > /dev/null` loops pinned to the same cores: with the loops, the process used 0.80 and 0.86 cores at a tick p50 of 1.25 ms; without them, 1.50 and 1.90 cores at 1.7 and 4.1 ms, and in CPU profiles of such a pair the same compression took 12.7 s against 8.0 s. The zstd process leaves most of its cores idle between ticks, and cores that keep falling idle do the same work in more time: through the CPU's power management (intel_pstate, powersave) or through the scheduler moving threads to idle cores, which these runs do not tell apart. brotli keeps three cores busy itself and varies less. Compare runs made under the same conditions.

The heap per stream is what the sse package measures: 1.68 MiB with zstd's full history, 1.56 MiB with brotli at quality 4 and a 2^18 window. 200 tabs hold 336 MiB or 312 MiB.

The writer kept up with 800 commands a second in every run, 1.3 to 2.4 to a transaction with FULL. A command's time from when it was due to its commit is mostly the fsync: its p50 was 2.5 to 5.9 ms. Its p99 depends on the disk, the host's shared SSD pool: 6 to 13 ms in these runs. On 2026-10-13, in two zstd runs while another agent's test suites ran, the writer waited up to 0.8 s for the disk and then took up to 512 commands (MaxBatch) into one transaction; fewer commits made fewer ticks (17.8 a second in the slowest run).

The 1-minute load average was 1.4 before the first set and 3.9 after it, 5.3 before the second and 5.0 after it, on 20 threads, the benchmark's own work included; the zstd levels (below) ran between the two sets.

### synchronous FULL against NORMAL

The same runs: `BenchmarkSharedBoard` runs each encoding with `synchronous=FULL`, then NORMAL. Medians of the six runs, the range in brackets:

| | FULL, zstd | NORMAL, zstd | FULL, brotli | NORMAL, brotli |
|---|---|---|---|---|
| CPU, cores busy | 1.82 (1.28 to 1.90) | 1.53 (0.89 to 1.67) | 3.42 (3.38 to 3.58) | 3.13 (3.10 to 3.22) |
| ticks a second | 19.8 | 19.8 (19.6 to 19.8) | 19.9 (19.8 to 19.9) | 19.8 |
| tick p95 | 3.6 ms (3.48 to 3.89) | 3.5 ms (1.63 to 3.58) | 1.8 ms (1.75 to 1.88) | 2.4 ms (2.29 to 2.46) |
| frames dropped | 0 | 0 | 0 | 0 |
| commands per transaction | 2.39 (1.69 to 2.64) | 1.03 (1.02 to 1.04) | 1.30 (1.25 to 1.40) | 1.02 (1.01 to 1.04) |
| command from due to commit, p50 | 5.5 ms (3.5 to 5.9) | 0.51 ms (0.51 to 0.53) | 2.7 ms (2.5 to 2.9) | 0.29 ms (0.29 to 0.31) |
| command from due to commit, p99 | 11 ms (9.5 to 13.1) | 29 ms (9.2 to 48.8) | 7.4 ms (6.4 to 7.8) | 12 ms (1.4 to 40.6) |

With NORMAL a command commits in 0.3 to 0.5 ms at p50; with FULL it waits for its batch's fsync, 2.7 to 5.5 ms, and the writer takes 1.3 to 2.4 commands into a transaction instead of one. The render loop does the same work in both modes: one render a tick, no dropped frames, and the CPU goes to the encoders.

The p99s compare poorly: they are the shared SSD pool's. On 2026-10-13, on lith's main at 8db2b69, a run with a disk stall waited for seconds in either mode (FULL with zstd once 2.7 s, at 9.4 ticks a second).

What NORMAL saves is latency, and what it costs is durability: a power loss or kernel crash can undo up to about 35 s of acknowledged commits, those since the writer's last checkpoint, with Linux's default writeback. A crash of the process loses none.

### zstd levels

Measured on 2026-10-14 between 17:24 and 17:26 UTC, between the two sets above, on the same machine and toolchain, on lith's main at ff7e536:

    taskset -c 0-11 go test ./loadtest -run '^$' -bench BoardZstdLevels -count 3

`BenchmarkBoardZstdLevels` (`loadtest/bench_test.go`) is `BenchmarkSharedBoard` with every stream on zstd, at SpeedFastest (lith's default), SpeedDefault and SpeedBetterCompression, with FULL. Medians of three runs each, the range in brackets:

| | fastest | default | better |
|---|---|---|---|
| bytes per frame on the wire | 2,213 (2,186 to 2,220) | 1,087 (1,076 to 1,129) | 365 (364 to 367) |
| CPU, cores busy | 1.84 (1.80 to 1.90) | 1.74 (1.71 to 1.76) | 3.15 (3.01 to 3.45) |
| heap per stream | 1.66 MiB | 2.76 MiB | 5.56 MiB |
| tick p99 | 4.14 ms (4.12 to 4.22) | 4.16 ms (4.10 to 4.22) | 3.33 ms (3.29 to 3.54) |

Every run sent 3,959 to 3,971 frames a second and dropped none. The default level halves the bytes of the fastest for no more CPU in these runs, and costs 1.1 MiB more per stream: about 220 MiB more for 200 tabs. Better compression sends a third of the default's bytes and fewer than brotli's 854 (above), for about brotli's CPU (3.42 cores) and three and a half times its heap. The 1-minute load average was 3.9 before the runs and 5.3 after them, the benchmark's own work included.

`TestHeapPerStream` (`sse/heap_test.go`) measures the same levels on one core, without the render loop: 200 frames of a 57 KB page, of which five rows change from one frame to the next, to each of 20 streams. Three runs on 2026-10-14 at 17:21 UTC, on lith's main at ff7e536, of

    go test ./sse -run TestHeapPerStream -v -count 3

gave the same bytes in each run:

| | bytes per frame | time per frame | heap per stream |
|---|---|---|---|
| zstd fastest | 405 | 29 to 30 µs | 1.65 MiB |
| zstd fastest, `ZstdLowMemory` | 405 | 40 to 42 µs | 1.03 MiB |
| zstd default | 228 | 23 to 25 µs | 2.73 MiB |
| zstd better | 109 | 119 to 122 µs | 5.54 MiB |
| brotli, quality 4 | 155 | 249 to 257 µs | 1.55 MiB |

The `memmove` in zstd's `addBlock` comes from its history: the encoder keeps twice its window (1 MiB for the 512 KiB window) and, when the next block does not fit, moves the newest 512 KiB to the front. With a 140 KB frame that is one 512 KiB move every three or four frames, as many bytes again as copying each frame in. No encoder option changes the ratio: a larger window moves more bytes less often, and `ZstdLowMemory` moves once for every frame.

### Streams that do not take their frames

`TestRenderCost` (`live/rendercost_unix_test.go`) measures what streams cost the hub when they do not take every frame: 50 tabs, each with a view of its own of about 34 KB that changes at every tick (`Every`, the default 50 ms tick), formatted row by row as a template would, on streams that keep up, on slow streams (`Spec.Slow`), and on streams whose client stopped reading. The clients take no bytes off the machine. Each kind runs for 11 s and is measured for the last 10. Measured on 2026-10-15 between 09:41 and 09:45 UTC on the same machine, with Go 1.27.2, not pinned (GOMAXPROCS 20), the 1-minute load average about 1.7, three runs each of lith's main at 9c0e842 and of the render-skip branch:

    LITH_RENDER_COST=1 go test ./live -run TestRenderCost -v -count=1

| Streams | Renders a second per stream, before | after | Cores busy, before | after |
|---|---|---|---|---|
| keep up | 19.8 | 19.8 | 2.17 to 2.20 | 2.17 to 2.19 |
| slow | 19.8 to 19.9 | 3.9 to 4.0 | 1.64 to 1.73 | 0.39 to 0.41 |
| stopped reading | 19.7 to 19.8 | 0 | 1.42 to 1.65 | 0.03 to 0.05 |

Before, a slow stream sent 4 frames a second and the hub rendered 20, dropping 16 of them; a stream whose client stopped reading sent none and the hub rendered 20, dropping all, until its write deadline (10 s by default, a minute here) ended it. With the render skip the hub defers those renders (15.7 and 19.7 a second per stream), and a view renders when its stream has taken the frame before. Streams that keep up cost the same.

## v0.4.0 against v0.3.1

Measured on 2026-10-19 between 14:14 and 14:27 UTC on the development machine of the Writer section, an Intel i5-13500 (20 threads) under Linux 6.18 (Unraid), with Go 1.27.2, modernc.org/sqlite v1.60.1, klauspost/compress v1.20.3 and andybalholm/brotli v1.2.6, the database in `/tmp`. v0.3.1 is the tag (eb4aa0f), v0.4.0 the `pre-040` branch at eb05371, before the release. Each test binary was built once, and six runs of each alternated, each run the four cases of `BenchmarkSharedBoard` (above), pinned to the performance cores:

    go test -c -o loadtest.test ./loadtest
    taskset -c 0-11 ./loadtest.test -test.run '^$' -test.bench SharedBoard -test.count 1

The 1-minute load average was 1.6 when the first run began; with the benchmark's own work it rose to 3.1 to 5.3 during the runs. The output of every run, with the load average at its start and end, is in `bench/v040/board.txt`. Medians of the six runs, the range in brackets:

| | FULL, zstd | NORMAL, zstd | FULL, brotli | NORMAL, brotli |
|---|---|---|---|---|
| CPU, cores busy, v0.3.1 | 1.84 (1.77 to 2.07) | 1.65 (1.60 to 1.70) | 3.35 (3.29 to 3.70) | 3.09 (3.05 to 3.17) |
| CPU, cores busy, v0.4.0 | 1.83 (1.78 to 1.87) | 1.62 (1.59 to 1.74) | 3.37 (3.35 to 3.83) | 3.08 (3.04 to 3.31) |
| tick p95, v0.3.1 | 3.8 ms (3.77 to 3.86) | 3.5 ms (3.43 to 3.59) | 1.9 ms (1.86 to 2.02) | 2.4 ms (2.06 to 2.46) |
| tick p95, v0.4.0 | 3.8 ms (3.67 to 4.00) | 3.5 ms (3.47 to 3.62) | 1.9 ms (1.80 to 2.03) | 2.4 ms (2.34 to 2.47) |
| bytes per frame, v0.3.1 | 2,234 (2,185 to 2,296) | 2,243 (2,180 to 2,305) | 855 (850 to 862) | 855 (846 to 865) |
| bytes per frame, v0.4.0 | 2,230 (2,220 to 2,288) | 2,296 (2,207 to 2,327) | 853 (849 to 862) | 858 (852 to 870) |
| commands per transaction, v0.3.1 | 2.44 (2.26 to 2.79) | 1.03 (1.03 to 1.04) | 1.32 (1.27 to 1.39) | 1.02 (1.01 to 1.04) |
| commands per transaction, v0.4.0 | 2.29 (2.23 to 2.41) | 1.03 (1.03 to 1.05) | 1.34 (1.29 to 1.35) | 1.03 (1.02 to 1.04) |
| command from due to commit, p50, v0.3.1 | 5.3 ms (5.0 to 6.3) | 0.52 ms (0.51 to 0.52) | 2.7 ms (2.6 to 2.8) | 0.30 ms (0.28 to 0.31) |
| command from due to commit, p50, v0.4.0 | 5.2 ms (5.0 to 5.8) | 0.52 ms (0.50 to 0.54) | 2.7 ms (2.6 to 2.8) | 0.30 ms (0.30 to 0.31) |
| command from due to commit, p99, v0.3.1 | 9.6 ms (8.8 to 11.2) | 31 ms (24 to 41) | 7.3 ms (6.8 to 10.3) | 14 ms (4 to 72) |
| command from due to commit, p99, v0.4.0 | 9.6 ms (8.5 to 13.2) | 31 ms (23 to 60) | 7.6 ms (7.2 to 14.2) | 28 ms (13 to 58) |

Nothing moved. Every run of both rendered the board once a tick at 19.8 to 19.9 ticks a second and dropped no frame, and the heap per stream was 1.66 to 1.68 MiB with zstd and 1.56 MiB with brotli in both. The medians differ by less than the runs of one version do; the p99s with NORMAL are the shared SSD pool's, as above. The board's view listens to its `board:<n>` and declares nothing with `Depends`: the topic tree costs each of its 800 commits a second about 60 ns, as the hub's benchmarks below show.

The hub's own benchmarks in `live` ran right after, the same way: six runs of each binary alternated, at a load average of 4.9 falling to 2.2 as the board's load wore off, against e9bebd0, the commit before the topic tree. Output in `bench/v040/hub.txt`:

    taskset -c 0-11 ./live.test -test.run '^$' -test.bench '^Benchmark(Changed|ChangedBelow|Tick|TickDepends)$' -test.benchmem -test.count 1

| | e9bebd0 | v0.4.0 |
|---|---|---|
| `Changed`, two topics on 1,000 views, depth 1 (`board7`) | | 150 ns |
| depth 2 (`board:7`; e9bebd0's `t:1`) | 127 ns | 190 ns |
| depth 3 (`src:0:7`) | | 217 ns |
| `ChangedBelow`, `src:A:*` over 1,000 of 2,000 views | | 48 µs |
| `Tick`, 1,000 views | 0.49 ms | 0.50 ms |
| `Tick`, 10,000 views | 4.64 ms | 4.71 ms |
| `TickDepends`, 1,000 views, the same topics | 0.50 ms | 0.53 ms |
| `TickDepends`, 1,000 views, four topics in and four out | 1.70 ms | 2.67 ms |

Medians; every run of a case was within 6% of its median but the moved `TickDepends`, 1.56 to 1.81 ms and 2.46 to 3.20 ms. A view that changes its topics now updates the hub's tree for each, about 2.1 µs a view for four in and four out against 1.2 µs before, with 4 more allocations; the rest moved by a few percent at most.

## After the v0.4.0 review: the writer and edge.Compress

Measured on 2026-10-20 between 13:07 and 13:51 UTC on the development machine of the Writer section, an Intel i5-13500 (20 threads) under Linux 6.18 (Unraid), with Go 1.27.2, modernc.org/sqlite v1.60.1, klauspost/compress v1.20.3 and andybalholm/brotli v1.2.6, the databases in `/tmp`. v0.4.0 is 7495607 (the tag with its modules' requirements); the changes are the branch `review-writer-perf`. Each test binary was built once, and the runs took `flock /tmp/lith-bench.lock`, pinned to the performance cores. Other agents' jobs shared the machine: the 1-minute load average fell from 25 to 3 during the first set (the large writes, the compression, the first writer runs, discarded for the second set) and stayed between 2.4 and 7.6 after. The output of every run, with the load average, is in `bench/writer-review/`.

### One command without a savepoint

A batch of one command runs it without `SAVEPOINT` and `RELEASE`, and rolls the batch back if it fails. `BenchmarkWriter`, ten runs of each binary alternating, 2 s a case, at a load average of 2.4 to 4.2:

    taskset -c 0-11 ./writer.test -test.run '^$' -test.bench 'Writer$' -test.benchtime 2s -test.count 1

| synchronous | producers | v0.4.0, commands/s | now | benchstat |
|---|---|---|---|---|
| FULL | 1 | 325 | 332 | ~ (p=1.00) |
| FULL | 50 | 6,785 | 6,840 | ~ (p=0.74) |
| FULL | 200 | 29,460 | 33,820 | ~ (p=0.05) |
| NORMAL | 1 | 25,240 | 26,560 | +5% (p=0.02) |
| NORMAL | 50 | 94,250 | 95,770 | ~ (p=0.25) |
| NORMAL | 200 | 86,960 | 86,820 | ~ (p=0.68) |

Medians. With 50 and 200 producers the batches hold 25 to 150 commands, and each keeps its savepoint, so nothing changes there; one producer runs six statements a command instead of eight, and gains 5%. One run of v0.4.0 and two of the branch at NORMAL with one producer stalled at 8,000 to 10,000 commands a second, the disk's doing. A `Do` of a command that touches a topic allocates 8 times instead of 19 (`TestDoAllocations`): BEGIN's and COMMIT's results in database/sql and the driver, the `Tx`, and the topics for `OnCommit`, where v0.4.0 added the request, its channel, `context.WithoutCancel`, a growing topic slice and the savepoint's two statements.

The savepoint cost most for the large write. `BenchmarkLargeWrite`'s update of 186 MB in one command, ten runs each:

| | v0.4.0 | now |
|---|---|---|
| write took | 0.47 s (0.45 to 0.54) | 0.33 s (0.31 to 0.39) |
| Do while it ran, max | 847 ms (794 to 1,255) | 682 ms (660 to 794) |
| longest gap between ticks | 843 ms (754 to 1,206) | 657 ms (632 to 790) |
| longest checkpoint | 369 ms | 357 ms |

The command alone in its batch now writes as fast as `Exclusive` did above (0.34 s): with a savepoint open, SQLite keeps a journal of every page the update changes, to roll back to it.

### Checkpoints beside the batches

`writer.Options.CheckpointAside` runs the passive checkpoints on a connection of their own (`sqlite.DB.Checkpointer`, with `wal_autocheckpoint(0)`) in a goroutine of their own, and the writer goes on with the next batch. SQLite starts the WAL over only at a write that finds it copied whole, which a checkpoint beside the batches rarely leaves, so once one has ended the writer copies what the batches wrote meanwhile itself, between two batches; without that, the WAL of `TestCheckpointAside` grew to 14 MB over 200 batches, every frame of them. `BenchmarkLargeWrite`, ten runs of each case, medians and ranges:

    taskset -c 0-11 ./loadtest.test -test.run '^$' -test.bench 'LargeWrite/NORMAL/(update|fts)/(one|chunks=1000)' -test.benchtime 1x -test.count 10

| write | as | checkpoints | write took | Do while it ran, p50 | max | longest gap between ticks | Do in the 2 s after, max | WAL at most | longest checkpoint |
|---|---|---|---|---|---|---|---|---|---|
| update | one command | between batches | 0.33 s | 520 ms | 682 ms | 657 ms | 360 ms | 186 MB | 357 ms |
| update | one command | aside | 0.34 s (0.33 to 0.54) | 172 ms | 343 ms (332 to 541) | 328 ms (284 to 532) | 131 ms | 187 MB | 253 ms |
| update | chunks of 1,000 | between batches | 1.08 s | 23.2 ms | 55 ms | 54 ms | 33 ms | 5.7 MB (4.6 to 9.0) | 35 ms |
| update | chunks of 1,000 | aside | 0.73 s (0.69 to 0.80) | 17.1 ms | 54 ms | 53 ms | 25 ms | 14.8 MB (12.4 to 24.8) | 36 ms |
| FTS5 rebuild | one command | between batches | 2.77 s | 1,530 ms | 2,903 ms | 2,877 ms | 137 ms | 56.5 MB | 125 ms |
| FTS5 rebuild | one command | aside | 2.87 s | 1,442 ms | 2,874 ms | 2,865 ms | 51 ms | 56.8 MB | 84 ms |

After a write in one command, the other commands now wait for the write alone, not for the checkpoint after it: the slowest waited 343 ms instead of 682, and the pages went 328 ms without a frame instead of 657. In chunks the update took a third less time, since a chunk no longer waits for the checkpoint of the one before, and the WAL grew to 15 MB instead of 6, by what the chunks wrote while a checkpoint ran. The FTS5 rebuild is one long statement either way. No run dropped a frame.

`BenchmarkWriterCheckpoint`, ten runs, at a load average of 4.2 to 7.6:

| synchronous | producers | between batches, commands/s | aside | p99 per transaction |
|---|---|---|---|---|
| NORMAL | 200 | 85,530 | 99,020 (+16%) | 5.1 ms, aside 3.9 ms |
| FULL | 200 | 34,870 | 33,030 (~) | 11.9 ms, aside 12.2 ms |

It is not the default. It opens a second connection to every database, which a `tenant` pool of many files doubles, and the WAL grows by what the batches write while a checkpoint runs: with sustained writes that is the write rate times the checkpoint's time, where the checkpoint between batches keeps it near `CheckpointPages`. Its gains matter for writes of tens of megabytes in one command, which the docs already say to split, and for the last 16% at NORMAL with hundreds of producers. An app that has such writes turns it on.

### edge.Compress: zstd before brotli

`edge.Compress` compresses every GET of a page, which is `no-store`, on every request; browsers send `gzip, deflate, br, zstd` at equal weight, and the earlier coding of `Negotiate`'s list wins a tie. `BenchmarkCompress` (`edge/compress_test.go`) compresses a board page of 156 KB, 560 cards in lists of 40, through `Compress` with each coding, ten runs:

| coding | time | output | throughput |
|---|---|---|---|
| brotli, quality 5, 2^18 | 1.41 ms | 7.0%, 11.0 KB | 106 MiB/s |
| zstd, default level, 2^18 | 0.165 ms | 8.2%, 12.8 KB | 904 MiB/s |
| gzip, default level | 0.233 ms | 8.9%, 13.8 KB | 640 MiB/s |

zstd takes an eighth of brotli's time for 1.9 KB more, so `Compress` now prefers it; Safari sends no zstd and keeps brotli. On four cores that is the difference between about 2,800 and 24,000 such pages a second of compression. `edge.Static` keeps its brotli -11 and gzip -9 copies, made once.

### database/sql's share, and SQLITE_OPEN_NOMUTEX

The review measured database/sql's plumbing as 70% of the writer's garbage and suggested running a batch on the driver's connection. That does not fit the writer as it is: `Tx.Query` and `Tx.QueryRow` return `*sql.Rows` and `*sql.Row`, which only database/sql makes, and `sql.Conn.Raw` holds the connection's lock while its function runs, so a batch inside one `Raw` cannot run a query through database/sql on the same connection. What remains is `Exec` on the driver's statement, one `Raw` per call. Ten runs of one UPDATE by id in a transaction (`bench/writer-review/raw-exec.txt`, a throwaway benchmark):

| | time | allocations |
|---|---|---|
| `*sql.Stmt.ExecContext` | 1.02 µs | 4, 133 B |
| `Raw` and the driver's `StmtExecContext`, arguments reused | 0.78 µs | 2, 21 B |

That is 0.24 µs per `Exec`, about 0.7 µs of the 11.5 µs a transfer of three `Exec`s and a `QueryRow` costs at 200 producers, 6%, for a second path that converts arguments as database/sql does (`driver.Valuer`, `NamedValueChecker`, `sql.Named`, the column converters `dbmetrics` passes on) and returns the driver's result. Not done: a small app runs hundreds of commands a second, not the benchmark's 87,000.

`SQLITE_OPEN_NOMUTEX` cannot be set: modernc.org/sqlite v1.60.1 opens every connection with `SQLITE_OPEN_FULLMUTEX` (`conn.go`, `newConn`), and SQLite's URI parameters have no mutex mode. The writer's connection is used by one goroutine, so the mutexes are never contended; their cost is the calls, which a fork of the driver would remove.

## The hub's own costs after the review of v0.4.0

Measured on 2026-10-20 between 12:55 and 13:07 UTC on the development machine above, an Intel i5-13500 (20 threads) under Linux 6.18 (Unraid), with Go 1.27.2, pinned to the performance cores. The base is v0.4.0's main at 7495607, the new binaries the review-live-perf branch at ddb8836. Each test binary was built once, and ten runs of each alternated, each with `-count 1` (`bench/live-perf/run.sh`); the output, with the load average at the start of every run, is in `bench/live-perf`. The machine was far from quiet: other agents ran test suites, and the 1-minute load average fell from 142 to 28 during the hub's runs and stayed between 18 and 30 for the others. Times vary by 25 to 50% between runs of one binary; allocations do not vary. Medians, by benchstat:

| | v0.4.0 | now | allocations a view, v0.4.0 | now | bytes a view, v0.4.0 | now |
|---|---|---|---|---|---|---|
| `Tick`, 1,000 views | 1.02 ms | 0.48 ms | 13.0 | 3.0 | 864 | 49 |
| `Tick`, 10,000 views | 9.11 ms | 6.53 ms | 13.0 | 3.0 | 864 | 48 |
| `TickDepends`, the same topics | 1.06 ms | 0.64 ms | 13.0 | 3.0 | 865 | 49 |
| `TickDepends`, four topics in and four out | 4.18 ms | 2.43 ms | 26.1 | 3.0 | 2,249 | 50 |

The same thirty runs again between 13:57 and 14:02 UTC, at a load average of 5 to 26 (`hub-*-2.txt`), gave the same allocations and shorter times on both sides: `Tick` 0.64 ms against 0.26 ms with 1,000 views and 5.47 ms against 3.06 ms with 10,000, `TickDepends` 0.60 ms against 0.29 ms with the same topics and 2.40 ms against 1.33 ms with topics that move, and e9bebd0 0.62 ms and 1.63 ms.

e9bebd0, before the topic tree, ran its `TickDepends` in the same rounds: 2.96 ms and 22.0 allocations a view with the topics that move, 1.21 ms with the same ones. A view whose topics move now allocates nothing in the tree, so the moved case costs what the same topics cost besides the tree's map work, and less than before the tree. `Changed` and `ChangedBelow` allocate nothing, as before, and did not get slower (181 to 308 ns at depth 1 to 3 against 289 to 354 ns, 31 µs against 77 µs below `src:A`; noise in either direction at this load).

The three allocations a view that remain are the benchmark's Render (its frame and the frame as an `any`) and the hub's record of the new frame. What went: a context with a timeout, its timer, an `AfterFunc` and two closures for every Render (each worker now ends its Renders' ctx with one timer it keeps), the sort of every offer in every tick (each stream now collects its own), and, for topics that move, a node, a map of views and a map of nodes below per topic, and the copy of the topics (the tree keeps up to four views of a topic in a slice and reuses the nodes and the maps that leave it, and a view's next topics fill a slice it keeps).

A frame that a Render formats with `sse.Elements` or `sse.AppendPatchElements` into `live.FrameBuffer(ctx)` is one buffer of the frame's size, where the HTML and then `sse.PatchElements` made two. The board of `BenchmarkSharedBoard` does so now (`loadtest/board.go`): with FULL and zstd, the ten runs of 12 s allocated 404 MiB each against 426 MiB, its tick p50 was 1.19 ms against 1.26 ms, and every other number stayed within its noise (1.03 cores, 2.2 KiB a frame, 1.68 MiB a stream, no frame dropped). In `sse`'s benchmarks of the 230 KB page, `AppendPatchElements` into a buffer of the right size and `PatchElements` take the same time and one allocation; `Elements` written row by row, as a template writes, takes about twice the time of either (123 µs against 66 µs), the price of a call for each of 2,309 rows of 102 bytes; a template that writes through a buffer of 4 KiB makes 60 times fewer calls.

A heartbeat on a hibernating zstd stream, with a budget (`BenchmarkHeartbeatAsleep`): woken for it and hibernated again, as live's streams sent it, 3.27 µs, 22 allocations and 2.9 KiB with the encoder in the pool; with `Stream.Ping` in raw blocks, 0.34 µs, 6 allocations and 175 bytes, and nothing changes in the budget. Once the garbage collector has emptied the pool, waking also builds an encoder of 0.6 to 1.7 MiB.

## Stream budget and hibernation

Measured on 2026-10-13 between 15:20 and 16:10 UTC on the same machine, with Go 1.27.2, klauspost/compress v1.20.3 and andybalholm/brotli v1.2.6, on lith's budget branch, pinned to the performance cores; the frame sizes, the views, the curve, the simulation, the load test and the pressure test again on 2026-10-14, on the budget-learn branch, with the 1-minute load average between 1.7 and 4.1; the settings and the floor again on 2026-10-14 between 17:28 and 17:34 UTC, on lith's main at ff7e536 (v0.2.0, whose streams flush their brotli encoders within a frame), with the 1-minute load average between 1.5 and 4.9. The tests in `bench/budget` run only with `LITH_BUDGET=1` and write the CSV files next to them:

    LITH_BUDGET=1 LITH_BUDGET_RUNS=3 taskset -c 0-11 go test ./bench/budget -run Settings -v -timeout 1h

Other agents' work kept the 1-minute load average between 3.3 and 5.6 while the CPUs stayed about 90% idle; during the load tests it rose to 6.6, with the test's own process at up to 4.5 cores.

### Settings

Two workloads. The board is `loadtest.Board`'s view rendered from SQLite: 200 frames of 142 KB, one card changed before each. The page is a synthetic tab of a small business app: lists of up to 72 entries, a form in a modal, a detail page and an overview, 150 frames of 2.4 to 36.9 KB (13.6 KB on average), most of them a view the tab saw before with a change. `TestSettings` writes the frames to 64 streams in turn, as the hub does, and measures the heap the open streams keep after two collections, divided by 64, the bytes per frame on the wire after the first frame, and the process's CPU time per frame, the median of three runs (the runs differ by up to 11%). zstd runs at its fastest level (`settings.csv`):

| Setting | Board: heap | bytes per frame | CPU per frame | Page: heap | bytes per frame | CPU per frame |
|---|---|---|---|---|---|---|
| zstd 2^15 | 1.62 MiB | 11,234 | 151 µs | 1.62 MiB | 1,130 | 19.5 µs |
| zstd 2^15, low memory | 0.53 MiB | 11,234 | 147 µs | 0.52 MiB | 1,130 | 17.8 µs |
| zstd 2^16 | 1.66 MiB | 10,872 | 149 µs | 1.65 MiB | 571 | 13.0 µs |
| zstd 2^16, low memory | 0.63 MiB | 10,872 | 146 µs | 0.58 MiB | 571 | 12.2 µs |
| zstd 2^17 | 1.66 MiB | 10,831 | 156 µs | 1.65 MiB | 379 | 11.0 µs |
| zstd 2^17, low memory | 0.69 MiB | 10,831 | 155 µs | 0.64 MiB | 379 | 10.8 µs |
| zstd 2^18 | 1.66 MiB | 2,703 | 63.5 µs | 1.65 MiB | 248 | 9.9 µs |
| zstd 2^18, low memory | 0.81 MiB | 2,703 | 72.0 µs | 0.76 MiB | 248 | 9.6 µs |
| zstd 2^19 | 1.66 MiB | 1,471 | 58.5 µs | 1.65 MiB | 189 | 9.0 µs |
| zstd 2^19, low memory | 1.06 MiB | 1,471 | 78.7 µs | 1.01 MiB | 189 | 10.2 µs |
| zstd 2^20 | 2.66 MiB | 794 | 52.4 µs | 2.65 MiB | 163 | 9.3 µs |
| zstd 2^20, low memory | 1.56 MiB | 794 | 116 µs | 1.51 MiB | 163 | 11.1 µs |
| brotli q3 2^16 | 0.67 MiB | 10,402 | 803 µs | 0.66 MiB | 266 | 51.1 µs |
| brotli q3 2^17 | 0.79 MiB | 10,454 | 799 µs | 0.78 MiB | 224 | 49.9 µs |
| brotli q3 2^18 | 1.04 MiB | 158 | 398 µs | 1.03 MiB | 183 | 49.2 µs |
| brotli q3 2^19 | 1.54 MiB | 162 | 386 µs | 1.53 MiB | 169 | 49.2 µs |
| brotli q3 2^20 | 2.54 MiB | 162 | 386 µs | 2.53 MiB | 166 | 51.4 µs |
| brotli q4 2^16 | 1.18 MiB | 9,903 | 956 µs | 1.17 MiB | 242 | 59.4 µs |
| brotli q4 2^17 | 1.30 MiB | 9,849 | 931 µs | 1.30 MiB | 201 | 58.7 µs |
| brotli q4 2^18 | 1.55 MiB | 95 | 394 µs | 1.55 MiB | 163 | 55.3 µs |
| brotli q4 2^19 | 2.05 MiB | 98 | 397 µs | 2.05 MiB | 150 | 55.2 µs |
| brotli q4 2^20 | 3.05 MiB | 99 | 394 µs | 3.05 MiB | 146 | 58.5 µs |
| gzip | 0.80 MiB | 13,169 | 143 µs | 0.79 MiB | 1,318 | 16.3 µs |
| identity | 1 KiB | 142,067 | 0.3 µs | 1 KiB | 13,703 | 0.3 µs |

- A window that cannot reach the frame before costs the board four times the bytes with zstd and a hundred times with brotli, and twice the CPU or more: below 2^18 a frame finds the previous one out of reach. The page loses less, since a window still holds several of its frames: against 2^19, it sends twice the bytes at 2^17, three times at 2^16 and six times at 2^15. A real business app's recorded frames lose less (9% more at 2^17, 19% at 2^16): its measured session repeats a few views more often than this page.
- zstd with its full history holds 1.6 MiB at every window up to 2^19, so below 2^19 only low memory saves heap. It costs CPU with the board's frames (79 µs against 59 at 2^19, 116 against 52 at 2^20) and 1 to 2 µs with the page's.
- On the board, brotli at quality 3 holds 0.51 MiB less than quality 4 at the same window, and once the window holds the frame sends two thirds more bytes for about the same CPU. At the same window, brotli takes five to seven and a half times the CPU of zstd with its full history on the board, four to six times on the page.

`TestFloor` (`floor.csv`, the page) finds what a compressed stream holds whatever its window: zstd with low memory holds 0.44 MiB at its smallest window, 2^10 (its 256 KiB hash table, 129 KiB of history and its block buffers), brotli at quality 2 or 3 0.56 MiB at windows up to 2^14, gzip 0.79 MiB. Below 0.44 MiB a stream only goes out uncompressed.

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

After 30 s, a sixth of the encoders stay and a frame takes about 1.1 KB more: 41% of the frames start a zstd frame without history (1,947 bytes on average), and the list after a modal finds only the modal in its frame. That is about 45 KB more per tab and hour. A heartbeat of a hibernating stream goes out in a frame of its own, 15 bytes instead of 6. Waking takes 36 to 62 µs for the page's 37 KB list with an encoder from the pool and 101 to 117 µs with a new one, 124 µs and 303 to 467 µs for the board's frame (`TestWakeCost`); the list then takes 3,790 bytes on the wire, the board 10,930. Run once more on 2026-10-14 at 17:33 UTC, on lith's main at ff7e536 (v0.2.0), the simulation gave the same bytes and the same heap within 0.001 MiB a tab, and waking took 34 and 103 µs for the list, 124 and 249 µs for the board.

Headless Chromium 154 (`TestChromiumDecodesFrames` in `sse`) decodes a zstd body of several frames event by event, each as soon as it is sent: frames that hibernation ended, raw frames while a budget had no room, frames from pooled encoders. Given a brotli body that holds a second brotli stream after the first, it decodes the first and drops the second without an error.

### A budget as streams grow

`TestSimulation` (`simulation.csv`) opens up to 2,000 streams of one workload and, after every ten, adds up the measured heap and bytes per frame of what each stream holds. The budgets are `sse.Budget` with MaxWindow 2^19 and MinWindow at the workload's floor (2^18 for the board, 2^17 for the page), since the simulated writes are small. Each stream writes once when it opens, which picks its tier; in all but "first write only" every open stream also writes after every ten opens, so that tiers shrink and waiting streams take the room that frees up. At 2,000 streams:

| Policy | Board: compressed | heap | bytes per frame | Page: compressed | heap | bytes per frame |
|---|---|---|---|---|---|---|
| lith's default, zstd 2^19 | 2,000 | 3,312 MiB | 1,471 | 2,000 | 3,294 MiB | 189 |
| zstd at the floor, low memory | 2,000 | 1,626 MiB | 2,703 | 2,000 | 1,278 MiB | 379 |
| budget of 128 MiB, first write only | 74 | 124 MiB | 136,865 | 74 | 123 MiB | 13,203 |
| budget of 128 MiB | 144 | 119 MiB | 132,037 | 167 | 109 MiB | 12,590 |
| budget of 128 MiB, 503 | 143, 1,857 refused | 116 MiB | 2,703 | 167, 1,833 refused | 107 MiB | 379 |
| budget of 1 GiB | 1,150 | 936 MiB | 61,937 | 1,337 | 855 MiB | 4,796 |

When each stream writes only its first frame, the first 73 take zstd 2^19 and leave no room for the rest. When streams that find no room make the others shrink, the budget holds as many streams as fit at the floor. A budget of 1 GiB gives the board's streams zstd 2^19 up to 590 streams, 2^19 with low memory up to about 900, 2^18 with low memory up to 1,150, and nothing beyond. The budget books 32 KiB for every open stream, 62 MiB at 2,000.

### The board with and without a budget

`TestLoadtest` (`loadtest.csv`) runs `BenchmarkSharedBoard`'s board in process, while 200 producers change cards 800 times a second, about 40 cards per frame. Two runs each:

| Streams | Policy | Cores busy | Bytes per frame | Heap | Frames dropped |
|---|---|---|---|---|---|
| 200 | lith's default | 1.74, 1.65 | 2,285, 2,199 | 334 MiB | 0 |
| 200 | zstd 2^18, low memory | 1.89, 1.89 | 3,908, 3,990 | 164 MiB | 0 |
| 200 | budget of 128 MiB: 141 at zstd 2^18 low memory, 59 raw | 1.64, 1.81 | 44,776, 44,789 | 116 MiB | 0 |
| 1,000 | lith's default | 3.80, 3.77 | 2,198, 2,221 | 1,668 MiB | 0 |
| 1,000 | zstd 2^18, low memory | 4.30, 4.32 | 3,984, 3,984 | 816 MiB | 0 |
| 1,000 | budget of 128 MiB: 124 and 123 compressed, the rest raw | 1.85, 1.82 | 125,434, 125,572 | 104 MiB | 0 |

Every run held about 19.8 ticks a second. The budget learns the board's 142 KB frame from the streams' first writes and gives no stream less than 2^18; with 1,000 streams, the 32 KiB it books for each take 31 MiB of the 128. The streams the budget has no room for send 142 KB per frame, 20 frames a second: with `Refuse` they would be answered 503 instead.

### Lowering the limit

`TestPressure` (`pressure.csv`): 500 board streams hold zstd 2^19 in a budget of 1 GiB: 867 MiB booked, 828 MiB of live heap. After `SetLimit(512 << 20)` each moves to 2^18 with low memory at its next frame: 445 MiB booked, and 407 MiB of live heap after a collection. That frame starts without history and takes 10,925 bytes.

### What a stream and a view hold

`TestViewCost` (`viewcost.csv`) streams data tables of 0.8, 49, 143 and 233 KB from a live hub to 64 streams that watch a view each, one view for every two, or one they all share. After a collection a view that two or more streams watch holds its newest frame rounded up to 8 KiB pages (56, 144 and 232 KiB) and about 1 KiB besides; a view that one stream watches holds no frame once the stream has written it (4 KiB of heap a stream at every size); a stream holds 3 to 7 KiB of heap and 8 to 10 KiB of stack in the hub; net/http holds 14 KiB of heap and 9 KiB of stack per open HTTP/1.1 connection. A render of the 233 KB table allocates 1,046 KiB, which the next collection frees. The budget books the frame of each view that two or more streams watch at its length and 8 KiB, 32 KiB per stream, and the encoder's tier: the most a stream held at it with frames of 50 to 230 KB (`framesize.csv`), rounded up to 16 KiB.

`TestBudgetMatchesHeap` in `live`, 12 streams with a view of their own of 146 or 246 KB, or one view for every two, on 2026-10-14:

| Frame | Streams a view | Coding | Booked | Frames booked | Live heap and stacks | Measured / booked |
|---|---|---|---|---|---|---|
| 146 KB | 1 | brotli q4 2^18 | 19,776 KiB | 0 KiB | 19,520 KiB | 0.99 |
| 146 KB | 1 | zstd 2^19 | 21,312 KiB | 0 KiB | 20,517 KiB | 0.96 |
| 146 KB | 1 | none | 384 KiB | 0 KiB | 256 KiB | 0.67 |
| 146 KB | 2 | brotli q4 2^18 | 20,680 KiB | 904 KiB | 20,091 KiB | 0.97 |
| 146 KB | 2 | zstd 2^19 | 22,216 KiB | 904 KiB | 21,336 KiB | 0.96 |
| 146 KB | 2 | none | 1,288 KiB | 904 KiB | 1,086 KiB | 0.84 |
| 246 KB | 1 | brotli q4 2^18 | 19,776 KiB | 0 KiB | 19,400 KiB | 0.98 |
| 246 KB | 1 | zstd 2^19 | 21,312 KiB | 0 KiB | 20,468 KiB | 0.96 |
| 246 KB | 1 | none | 384 KiB | 0 KiB | 317 KiB | 0.83 |
| 246 KB | 2 | brotli q4 2^18 | 21,263 KiB | 1,487 KiB | 20,734 KiB | 0.98 |
| 246 KB | 2 | zstd 2^19 | 22,799 KiB | 1,487 KiB | 21,904 KiB | 0.96 |
| 246 KB | 2 | none | 1,871 KiB | 1,487 KiB | 1,658 KiB | 0.89 |

The test's streams have no net/http connection, which the 32 KiB per stream include.

`TestViewWithOneStreamKeepsNoFrame` in `live` sends an 8 MiB frame to one stream: once the frame went out, uncompressed, with zstd or with brotli, what the hub holds is within 120 KiB of what it held before; while a second stream watches the view, the hub holds the frame.

`TestJoinCost` (`joincost.csv`) joins 200 streams one after another to a data table's view while one stream watches it, whose frame the hub dropped, or two, whose frame it keeps; the CPU is the median of three runs:

| Frame | Streams watching | Renders per join | CPU per join | Allocated per join | Booked for the frame |
|---|---|---|---|---|---|
| 49 KB | 1 | 1 | 347 µs | 167 KiB | none |
| 49 KB | 2 | 0 | 71 µs | 8 KiB | 56 KiB |
| 143 KB | 1 | 1 | 716 µs | 562 KiB | none |
| 143 KB | 2 | 0 | 73 µs | 8 KiB | 147 KiB |
| 233 KB | 1 | 1 | 1,193 µs | 1,056 KiB | none |
| 233 KB | 2 | 0 | 69 µs | 8 KiB | 235 KiB |

Hashing a frame for the unchanged-frame check takes 3.5 µs at 49 KB and 15.8 µs at 233 KB.

`TestWindowMargin` (`margin.csv`): at 2^18 brotli refers to the frame before up to a frame of 259 KB, 99% of the window (249 bytes a frame at quality 3), and not at 266 KB (29,719 bytes). The budget wants a window that holds the largest write with a sixteenth to spare.

### The curve with real frames

`TestBudgetCurve` (`curve.csv`, and `curve.md`, the table on the docs' budget page) opens streams one by one on a hub with real encoders and frames, with a write to every stream after every ten opens. At 150 MiB:

| Frame | Views | Clients | Opened | Window holds the frame | Compressed | First uncompressed | Booked | Heap |
|---|---|---|---|---|---|---|---|---|
| 49 KB | one | brotli | 300 | 163 | 163 | 164th | 154.0 MiB | 141.0 MiB |
| 49 KB | one | zstd | 300 | 213 | 213 | 88th | 152.5 MiB | 130.5 MiB |
| 233 KB | one | brotli | 300 | 114 | 114 | 115th | 155.5 MiB | 146.4 MiB |
| 233 KB | one | zstd | 300 | 168 | 168 | 88th | 154.0 MiB | 141.8 MiB |
| 233 KB | one each | brotli | 150 | 114 | 114 | 115th | 150.6 MiB | 144.7 MiB |
| 233 KB | one each | zstd | 150 | 150 | 150 | 88th | 133.6 MiB | 125.0 MiB |
| 233 KB | one each | half each | 150 | 124 | 124 | 102nd | 135.2 MiB | 128.2 MiB |
| 233 KB | one each | zstd | 200 | 168 | 168 | 88th | 150.6 MiB | 140.4 MiB |

The hub keeps no frame of a view that one stream watches, so the tabs book no frames. 150 tabs with a 233 KB view of their own all hold 2^18 in 200 MiB with brotli (197.4 MiB booked, 190.5 MiB of heap), in 140 MiB with zstd (133.6 and 125.1 MiB), and in 230 MiB half each. With `Refuse`, every tab let in holds 2^18: 120 MiB lets in 134 of the 150 zstd tabs, 91 brotli tabs and 111 when half are each; 150 MiB all 150 zstd tabs, 114 brotli tabs and 139 mixed.

### What lith adopted

A live hub makes a budget when it is given none: half the process's memory limit, for the streams' encoders, 32 KiB per stream and the frames of views that two or more streams watch, and no change at all without a memory limit (a stream's own ZstdWindow and BrotliWindow cap what it gets). A stream's first write picks a window that holds its largest write. Brotli takes that window at quality 4 while the budget stays at most half full, at quality 3 after, and 2^16 only when that does not fit; zstd takes the largest tier within its fair share and moves down while open, no further than its frame's window. A brotli stream below its frame's window, or uncompressed, is asked to open again once room frees up. Pages with large frames pass a budget with `Refuse`. The hub keeps the frame of a view that two or more streams watch, which a join to it would otherwise render again; of a `[]byte` view that one stream watches, only the frame's length and hash. Hibernation is opt-in: `live.Options.Hibernate` is zero unless an app sets it.

## Minified static files

Measured on 2026-10-16 on the same machine, an Intel i5-13500 (20 threads), with Go 1.27.2 and tdewolff/minify v2.24.19, on lith's minify branch (on 3fcebd5). The sizes come from the tests that check minification, run with `-v`:

    cd minify && go test -run TestStatic -v .
    cd site && go test ./internal/app -run TestMinified -v

`minify`'s `TestStatic` serves the template's static files through `edge.Static` with `edge.Minify(minify.Bytes)`, `newest-frame.js` added as the template adds it. The site's `TestMinified` loads the scripts and stylesheets of a demo page, the Game of Life, from the site in production and with `-dev`. Bytes as written against minified, raw and under brotli at 11:

| | as written | minified | brotli, as written | brotli, minified |
|---|---|---|---|---|
| template: `style.css` | 1,600 | 1,303 | 569 | 514 |
| template: `lith/newest-frame.js` | 7,352 | 2,767 | 2,620 | 1,332 |
| template: `vendor/datastar-rocket.js` | 68,224 | 68,224 | 21,958 | 21,958 |
| template, together | 77,176 | 72,294 | 25,147 | 23,804 |
| site: `site.css`, the bundle | 211,704 | 162,378 | 34,382 | 23,426 |
| site: `js/transitions.js` | 6,104 | 3,304 | 2,099 | 1,353 |
| site: `lith/newest-frame.js` | 7,352 | 2,767 | 2,620 | 1,332 |
| site: Datastar and two Starbase bundles | 77,278 | 77,278 | 25,818 | 25,818 |
| site, together | 302,438 | 245,727 | 64,919 | 51,929 |

Under brotli, minifying saves the template's page 1,343 bytes, 5% of what it loads, all of it from its stylesheet and the plugin, and the site's demo page 12,990 bytes, 20%, most of it from the CSS bundle. The site minified these files before, with code of its own that `lith/minify` replaces. The vendored builds go out as written: tdewolff takes 290 bytes, 0.4%, off the Datastar build and 3 to 52 bytes off each Starbase bundle, less than the twentieth that `edge.Static` asks for, and Static does not ask for `*.min.js` files at all. SVG and JSON stay as written: tdewolff's minifiers take the site's logo from 855 to 789 bytes under brotli, and `lock.json`, which no page loads, from 1,142 to 1,098; the template serves neither.

`BenchmarkNewStatic` (`minify/static_test.go`) runs `edge.NewStatic` on the template's 3 static files and the site's 37, read into memory first as `go:embed` holds them, and adds `newest-frame.js`, with and without `Minify`. Pinned to the performance cores, six runs at 12:42 UTC, while another agent profiled on the machine (1-minute load average 4.4 before, 4.1 after):

    cd minify && taskset -c 0-11 go test -run '^$' -bench NewStatic -benchmem -count 6 -benchtime 2s .

| | as written | minified | allocated, minified |
|---|---|---|---|
| template | 0.12 ms (0.08 to 0.13) | 6.1 ms (5.4 to 6.4) | 2.2 MB |
| site | 0.52 ms (0.40 to 0.58) | 9.2 ms (9.0 to 10.0) | 3.6 MB |

Medians, the range in brackets. Most of the template's time is the Datastar build: `minify.Bytes` takes 3.5 ms for its 68 KB, 19 MB/s, after which Static serves it as written; the plugin takes 0.11 ms and the stylesheet 0.04 ms. A build named `*.min.js` costs nothing, since Static skips it before parsing. The site minifies its CSS bundle in `Add` besides, 2.1 ms for its 212 KB when the machine was quieter, which it did before as well.

The template's binary, built as its Taskfile builds it (`CGO_ENABLED=0 go build -trimpath -ldflags '-s -w'`), is 14,008,480 bytes with `lith/minify` and 13,349,024 without, from the same generated project with the import and the option taken out: the module adds 644 KiB, 4.9%. A project that does not import it builds as before; tdewolff/minify is in the template's module graph anyway, through air's dependency on Hugo, but never in its binary.
