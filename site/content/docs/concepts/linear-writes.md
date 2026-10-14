---
title: Linear writes
headline: Linear writes: one writer applies every change
summary: One writer applies every change, in the order the changes arrive, many to a transaction with a savepoint each.
description: One writer applies every change in arrival order, many commands to a transaction with a savepoint each, so a failed command undoes only its own changes.
order: 1
---

SQLite lets one connection write at a time. An app that writes from every request has its goroutines queue for that lock, and under load some of them wait past the busy timeout and fail with `SQLITE_BUSY`. lith gives the write connection to one goroutine, the writer, and every change becomes a command that the writer applies.

## Commands

A command is a value with an `Apply(tx *writer.Tx) error` method, or a `writer.Func`. It reads and writes through the transaction it gets, and names the topics it changed with `tx.Touch`. Checks that read before they write go inside `Apply`: the command sees every command before it, so there is no optimistic retry loop.

```go
func (c MoveCard) Apply(tx *writer.Tx) error {
	var board, version int64
	err := tx.QueryRow(`SELECT board_id, version FROM cards WHERE id = ?`, c.Card).Scan(&board, &version)
	if err != nil {
		return err
	}
	if version != c.Seen {
		return ErrMovedMeanwhile // nothing changed; the page says why
	}
	if _, err := tx.Exec(`UPDATE cards SET lane_id = ?, version = version + 1 WHERE id = ?`, c.Lane, c.Card); err != nil {
		return err
	}
	tx.Touch(fmt.Sprintf("board:%d", board))
	return nil
}
```

A handler hands the command over in one of three ways:

- `w.Do(ctx, cmd)` waits until the command's batch has committed and returns the command's own error.
- `w.DoValue(ctx, fn)` does the same for a function that returns a value, such as the id of a new row.
- `w.Send(cmd)` returns at once. Nobody waits for the outcome, so a failure goes to `Options.OnSendError`, or else to the log.

## Batches with a savepoint each

While a batch runs, new commands queue up. The next batch takes all of them, up to 512, into one `BEGIN IMMEDIATE` transaction, each command in a savepoint of its own. A command that fails rolls back to its savepoint, and its error goes to its `Do`; the others commit together. The batch needs one commit, and with `synchronous=FULL` one fsync.

On the VPS that serves this site, where an fsync takes about 7 ms, one producer with FULL gets 143 commands a second, each waiting for its own fsync. Two hundred producers get 14,480 a second, 108 to a transaction. With NORMAL, the default, the same two hundred get 80,530 a second. On a development machine with a faster disk (an Intel i5-13500) FULL gives 526 and 34,893. The writer needs no tuning for that: under load, more commands share each commit.

## Order

Commands apply in the order they arrive, from `Send` and `Do` alike. `w.Flush(ctx)` returns once every command queued before it is applied, so a test can send, flush and read.

## What a command must not do

A command never waits for the network and never renders: every command queued behind it waits while it runs. Mail, HTTP calls and anything slow go into `tx.AfterCommit`, which runs once the batch has committed, or outside the writer.

## Retries and refusals

A request that the network repeats should not apply twice. `writer.Once(key, cmd)` records the key of a command that succeeded, and a command with a recorded key succeeds without running again. `writer.OnceResult` also records a refusal (`*writer.Refusal`) with its message, so a retry gets the first answer. `writer.PruneOnce(age)` forgets old keys; send it from a sweep.

## Statements

The writer prepares each SQL text once on its connection and keeps the 512 used last: modernc's SQLite parses and plans every statement that has no prepared one, and that took 41% of kanban's writer CPU before the cache. Build SQL from constants. SQL built at runtime, such as an `IN` list with a mark per value, goes through `tx.ExecUncached`, `tx.QueryUncached` or `tx.QueryRowUncached`, which keep nothing.

## Work a transaction cannot hold

`PRAGMA foreign_keys` does nothing inside a transaction, and SQLite refuses `ATTACH`, `DETACH` and `VACUUM` there. `w.Exclusive(ctx, fn)` runs fn on the writer's own connection between two batches, outside any transaction. Commands queued meanwhile wait and then run in order, and fn tells the hub what changed itself, since no `OnCommit` runs for it.
