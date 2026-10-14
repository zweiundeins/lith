---
title: The writer
headline: Writer benchmarks: commands a second, FULL and NORMAL
summary: Commands a second with synchronous FULL and NORMAL, at 1, 50 and 200 producers, what a transaction costs, and what checkpoints do to the slowest commits.
order: 1
---

`BenchmarkWriter` (`writer/bench_test.go`) runs the writer alone, without HTTP: 1, 50 or 200 producers, each moving money between ten accounts of its own and waiting for the commit with `Do`, as a request handler does. A command reads a balance, debits one account, credits another, appends to a ledger and touches a topic: four statements, plus the writer's `SAVEPOINT` and `RELEASE`. SQLite checkpoints the WAL itself (`CheckpointAuto`). Measured on 2026-10-12, medians of three runs.

```chart
kind: bars
title: Commands a second, by producers waiting with Do
x: producers
y: commands a second
source: bench/RESULTS.md
---
FULL,1,526
FULL,50,13260
FULL,200,34893
NORMAL,1,13049
NORMAL,50,63267
NORMAL,200,66688
```

| synchronous | Producers | Commands a second | Commands per transaction | ms per transaction |
| --- | --- | --- | --- | --- |
| FULL | 1 | 526 (511 to 579) | 1 | 1.9 |
| FULL | 50 | 13,260 (12,357 to 14,025) | 25 | 1.9 |
| FULL | 200 | 34,893 (27,317 to 37,044) | 122 | 3.5 |
| NORMAL | 1 | 13,049 (11,199 to 15,793) | 1 | 0.07 |
| NORMAL | 50 | 63,267 (62,423 to 79,296) | 26 | 0.39 |
| NORMAL | 200 | 66,688 (63,440 to 77,502) | 109 | 1.4 |

A transaction's time runs from `BEGIN IMMEDIATE` to the end of `COMMIT`.

## FULL waits for the disk

With `synchronous=FULL`, a producer alone waits for one fsync per command, 1.9 ms on this disk. Fifty producers share each fsync, 25 commands at a time, and get 25 times as far; at 200 a transaction takes 122 commands. The writer needs no setting for that: while a batch commits, the next commands queue, and the next batch takes all of them.

## NORMAL is bound by the CPU

With NORMAL the writer costs about 13 µs per command at 200 producers. In a CPU profile of that case, `sqlite3_step` takes 42% of the process's samples, the rest of the writer's goroutine (database/sql, the driver's binding, locks) 32%, and the garbage collector 8%. The writer's own code, its queue, savepoints and statement cache, is under 2%.

What NORMAL costs is durability. It syncs the WAL only at checkpoints, so a power loss or a kernel crash can undo the commits since the last one, up to about 35 s of them with Linux's default writeback (`vm.dirty_expire_centisecs` 3000, flushed every 5 s). A crash of the process loses none. lith defaults to NORMAL; set `sqlite.Options.Synchronous` to `"FULL"` where an acknowledged commit must survive a power loss, at one fsync per batch.

## Checkpoints

At 200 producers with `CheckpointAuto`, the slowest 1% of transactions take 7 to 26 ms: SQLite checkpoints inside the `COMMIT` that brings the WAL past 1000 pages. `BenchmarkWriterCheckpoint` runs the same case with `sqlite.CheckpointWriter`, where the writer checkpoints after it has answered the batch:

| synchronous | Producers | Commands a second | Per transaction | ms per transaction | p99 ms per transaction |
| --- | --- | --- | --- | --- | --- |
| FULL | 200 | 45,100 (44,075 to 46,228) | 125 | 2.6 | 4.6 to 5.3 |
| NORMAL | 200 | 71,381 (68,629 to 74,217) | 102 | 1.1 | 3.0 to 3.8 |

The checkpoint is no longer part of a transaction's time: the producers queue their next commands while it runs. The template and this site open their databases with `CheckpointWriter`.

## A real command

kanban's `BenchmarkMoveCard` on the same machine, on 2026-10-08 with FULL, moved 240 to 340, about 5,100 and about 13,800 cards a second with 1, 50 and 200 people, in transactions of 1, 25 and 103 moves. A move runs about fifteen statements, and a transaction alone took 2.9 to 4.2 ms. Before the writer prepared each SQL text once, modernc's parsing and planning took 41% of the writer's CPU at 200 people, and the same run moved 10,000 cards a second.

Anders Murphy's benchmark, two updates per transaction on an M1 MacBook, reports 98,163 transactions a second with a savepoint each and FULL. Without `PRAGMA fullfsync`, macOS's fsync does not wait for the drive's cache, so NORMAL is the closer comparison here. His workload is smaller than a real command: the shape carries over, the figures do not.

## Measuring again

```sh
taskset -c 0-11 go test ./writer -run '^$' -bench Writer -benchtime 5s -count 3
```

`-bench Writer` runs `BenchmarkWriter` and `BenchmarkWriterCheckpoint`, each with FULL and NORMAL.
