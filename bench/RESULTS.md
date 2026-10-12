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
