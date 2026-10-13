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
