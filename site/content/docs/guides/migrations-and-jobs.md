---
title: Migrations, jobs and imports
nav: Migrations and jobs
headline: Migrations, background jobs, imports and sweeps
summary: The schema as numbered SQL files, work a transaction cannot hold, imports, large updates and deletes, background jobs, sweeps and checkpoints.
order: 3
---

## Migrations

The schema lives in numbered files, `NNN_name.sql`, embedded into the binary. `sqlite.Open` applies the files numbered above the database's `PRAGMA user_version` in order, each in a transaction of its own, and sets `user_version` to the file's number with it:

```go source=site/internal/docs/example/board/migrations/migrations.go
// Package migrations holds the board's schema: NNN_name.sql files that
// sqlite.Open applies in order, each once.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
```

```go
db, err := sqlite.Open(ctx, "data/board.db", sqlite.Options{Migrations: migrations.FS})
```

While the files run, foreign keys are off, as SQLite's procedure for changing a table's schema asks: rebuilding a table would otherwise cascade the deletes of its old copy. `PRAGMA foreign_key_check` runs before each file commits instead, and a violation fails the migration. A database whose `user_version` is above the newest file is refused: an older binary does not run on a newer schema, which matters on a rollback.

Rules that keep this working:

- Add a file for every change. Never edit a file that has shipped: databases that ran it never run it again.
- Two branches must not use the same number for different files. The second one to merge renumbers.
- Keys are `INTEGER PRIMARY KEY`, or time-ordered. A random key, such as a UUIDv4, scatters inserts over the table's B-tree; keep random tokens on small tables. A table keyed by several columns is `WITHOUT ROWID`, like the board's `tab_state`:

```sql source=site/internal/docs/example/board/migrations/002_tabs.sql
CREATE TABLE tab_state (
    sid    TEXT NOT NULL,
    tab_id TEXT NOT NULL,
    data   TEXT NOT NULL,    -- JSON of TabState
    seen   INTEGER NOT NULL, -- unix seconds, for the sweep
    PRIMARY KEY (sid, tab_id)
) WITHOUT ROWID;
```

A project that keeps its own migration runner, with a `schema_migrations` table say, passes it as `Options.Migrate` instead of `Migrations`. `Open` calls it with the write connection before the read pool opens; a runner that turns `foreign_keys` off must turn it on again, since the writer takes that connection over. `sqlite.Migrate(ctx, db, fsys)` is the built-in runner on its own, for a test or a tool.

## Work a transaction cannot hold

`PRAGMA foreign_keys` does nothing inside a transaction, and SQLite refuses `ATTACH`, `DETACH` and `VACUUM` there. A command runs inside the batch's transaction, so such work goes through `w.Exclusive(ctx, fn)`: the writer finishes the batch it is in, then runs `fn` on its own connection, outside any transaction. Commands that arrive meanwhile wait and run after it, in order.

```go source=site/internal/docs/example/board/commands.go
func ImportFile(ctx context.Context, w *writer.Writer, path string) error {
	return w.Exclusive(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS src`, path); err != nil {
			return err
		}
		defer conn.ExecContext(context.WithoutCancel(ctx), `DETACH DATABASE src`)
		tx, err := conn.BeginTx(ctx, nil) // BEGIN IMMEDIATE, as every transaction on the write connection
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, q := range []string{
			`INSERT INTO boards SELECT * FROM src.boards`,
			`INSERT INTO lanes SELECT * FROM src.lanes`,
			`INSERT INTO cards SELECT * FROM src.cards`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}
```

What `fn` has to do:

- Restore every pragma it changes and detach what it attaches, on its error paths too. A `defer` with `context.WithoutCancel(ctx)` still runs when the request that started it is gone.
- End the transactions it begins. One left open is rolled back, and `Exclusive` returns an error that says so.
- Never wait for the writer: `Do`, `Flush` or another `Exclusive` inside `fn` waits for the writer, which waits for `fn`.
- Tell the views what changed. `Exclusive` reaches no `OnCommit`, so call `hub.Changed` with the topics, or `hub.MarkAll()`, once it returns.

`fn` may change the schema: the writer prepares its statements again afterwards. If `ctx` ends before `fn`'s turn, `fn` does not run; once it has begun, `Exclusive` waits for it. A second connection that writes beside the writer is the thing `Exclusive` replaces: a transaction on it that runs past the busy timeout makes the writer's batches fail with `SQLITE_BUSY`.

Every command waits while `fn` runs, so an import that a command's transaction can hold goes through commands instead.

## Bulk imports

A large import goes to the writer in chunks of a few thousand rows, each a command, so the commands of the people using the app get their turn between two chunks. Sort each chunk by its primary key first: sorted keys append to the table's B-tree, where random ones would land all over it.

```go source=site/internal/docs/example/board/commands.go
// Import inserts cards in commands of 2,000, sorted by id: other commands
// get their turn between two chunks, and sorted keys append to the
// table's B-tree where random ones would land all over it.
func Import(ctx context.Context, w *writer.Writer, cards []Card) error {
	slices.SortFunc(cards, func(a, b Card) int { return cmp.Compare(a.ID, b.ID) })
	for chunk := range slices.Chunk(cards, 2000) {
		if err := w.Do(ctx, ImportCards{Cards: chunk}); err != nil {
			return err
		}
	}
	return nil
}
```

Each chunk is a transaction of its own. An import that must land whole or not at all is one command, or one `Exclusive`.

## Large updates and deletes

Some writes change most of a large table at once: deleting the rows a full pass of a sync did not see, clearing every cached rendering before a one-time re-render, rebuilding a full-text index. One statement does it, but the writer runs one thing at a time, so every other command waits until it ends, and then for the checkpoint of what it wrote. Split such a write into commands by rowid range, sent one after the other:

```go
const chunk = 1000 // rows of about 4 KB: about 24 ms of writing each
var last int64
if err := db.R.QueryRowContext(ctx, `SELECT coalesce(max(id), 0) FROM comments`).Scan(&last); err != nil {
	return err
}
for lo := int64(1); lo <= last; lo += chunk {
	part := writer.Func(func(tx *writer.Tx) error {
		_, err := tx.Exec(`UPDATE comments SET html = NULL WHERE id BETWEEN ? AND ? AND html IS NOT NULL`, lo, lo+chunk-1)
		tx.Touch("comments")
		return err
	})
	if err := w.Do(ctx, part); err != nil {
		return err
	}
}
```

`BenchmarkLargeWrite` measured what each way costs, on 2026-10-19 on the development machine with NORMAL: a table of 45,000 comments of about 4 KB each, 185 MB, while 200 producers sent 800 commands a second and 200 streams watched a board that rendered in every tick ([the measurements](/docs/performance/writer#large-updates-and-deletes)).

- **One command** cleared every comment's HTML in 0.49 s, then the writer checkpointed the 186 MB it had written to the WAL, for another 0.38 s. Other commands waited up to 0.88 s, half of them more than 0.63 s. The hub kept ticking, but without commits it had nothing new to render, so the pages got no frame for up to 0.87 s. The WAL stayed at 186 MB: the writer truncates it only after a second without commands.
- **Commands of 1,000 rows** took 1.07 s in all, twice as long, and no other command waited longer than 45 ms. The board rendered in every tick, the longest gap between two frames 51 ms, and the WAL stayed at 8.3 MB. Commands of 5,000 rows took 0.89 s, held other commands up to 163 ms and the frames about 150 ms apart.
- **`Exclusive`** with one transaction wrote the update in 0.34 s, and held commands and pages as long as one command does.

Start with a chunk that writes for about one tick, 50 ms, and size it by bytes rather than by rows: a thousand rows of 4 KB take about 24 ms. With `synchronous=FULL` the same chunks cost each waiting command a few milliseconds more.

A delete can be far cheaper than an update of the same rows: deleting all 45,000 comments in one command took 102 ms, and the WAL did not grow. Measure the app's own delete before splitting it; in commands of 5,000 rows, other commands waited at most 18 ms.

`Exclusive` is right when the change must land whole and a pause is acceptable, such as a step of a deploy or a maintenance window, and for work a transaction cannot hold. It reaches no `OnCommit`, so call `hub.Changed` or `hub.MarkAll()` once it returns. A chunked write is visible half done between its chunks; one that must not be is one command or one `Exclusive`.

An FTS5 index rebuild (`INSERT INTO fts (fts) VALUES ('rebuild')`) of the 45,000 comments took 2.77 s, one statement with no range: every command waited, and the pages got no frame for 2.9 s. Where that pause is too long, fill a new index in rowid chunks instead; the benchmark does not measure that.

## Background jobs

A job does its slow part outside the writer and hands the result over as a command. The board's mail sender reads the outbox from the read pool, sends each mail over the network, and deletes its row with a command:

```go source=site/internal/docs/example/board/outbox.go
// Run sends what the outbox holds whenever Wake is called, and once a
// minute besides, until ctx ends. Start it from main, like every job.
func (o *Outbox) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if err := o.flush(ctx); err != nil && ctx.Err() == nil {
			o.log.Warn("outbox", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-o.wake:
		case <-t.C:
		}
	}
}
```

```go source=site/internal/docs/example/board/outbox.go
for _, m := range waiting {
	if err := o.send(ctx, m.to, m.subject); err != nil {
		return err
	}
	sent := writer.Func(func(tx *writer.Tx) error {
		_, err := tx.Exec(`DELETE FROM outbox WHERE id = ?`, m.id)
		return err
	})
	if err := o.w.Do(ctx, sent); err != nil {
		return err
	}
}
```

A mail whose row the job could not delete goes out again on the next pass: the receiver may get it twice, and never zero times. Start every job from `main`, on a context that ends at shutdown, and wait for it before the writer closes. A job that nothing starts does nothing.

## Sweeps

Every table that grows needs a rule for what it keeps, and a sweep that enforces it. The board's sweep forgets quiet tabs, old preferences and long-archived cards, with their event streams ([Event sourcing](/docs/guides/events#retention) has `forgetCards`):

```go source=site/internal/docs/example/board/commands.go
func (c Sweep) Apply(tx *writer.Tx) error {
	month := c.At.AddDate(0, -1, 0).Unix()
	if err := forgetCards(tx, month); err != nil {
		return err
	}
	for _, q := range []struct {
		sql string
		arg int64
	}{
		{`DELETE FROM tab_state WHERE seen < ?`, c.At.Add(-TabTTL).Unix()},
		{`DELETE FROM prefs WHERE seen < ?`, c.At.AddDate(-1, 0, 0).Unix()},
		{`DELETE FROM card_history WHERE card_id IN (SELECT id FROM cards WHERE archived < ?)`, month},
		{`DELETE FROM cards WHERE archived < ?`, month},
	} {
		if _, err := tx.Exec(q.sql, q.arg); err != nil {
			return err
		}
	}
	return nil // archived cards show on no board: no topic changed
}
```

A sweep that deletes rows a page shows touches their topics, so the pages drop them in the next tick. The template starts its sweep from `main`, every hour, together with `writer.PruneOnce`, which forgets the op ids of [`Once` and `OnceResult`](/docs/guides/commands#op-ids-and-retries):

```go source=template/cmd/app/main.go
func sweep(ctx context.Context, w *writer.Writer, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, cmd := range []writer.Command{guestbook.Sweep{At: time.Now()}, writer.PruneOnce(24 * time.Hour)} {
			if err := w.Do(ctx, cmd); err != nil && ctx.Err() == nil {
				log.Warn("sweep", "err", err)
			}
		}
	}
}
```

## Checkpoints and optimize

In WAL mode, SQLite appends commits to the `-wal` file and a checkpoint copies them back into the database file. `sqlite.Options.Checkpoint` says who does that:

| Mode | Who checkpoints | For |
| --- | --- | --- |
| `CheckpointAuto` (the default) | SQLite, inside the commit that brings the WAL past 1000 pages | tools and tests |
| `CheckpointWriter` | the writer, between batches: a passive checkpoint every 1000 pages written, and one that truncates the WAL after a second without commands | apps without Litestream |
| `CheckpointExternal` | another process, such as Litestream | apps that replicate ([Backups](/docs/guides/backups)) |

With `CheckpointAuto`, the commit that runs the checkpoint takes longer: in lith's writer benchmark the slowest 1% of transactions took 7 to 26 ms, against 3.0 to 5.3 ms with `CheckpointWriter` ([Performance](/docs/performance/writer#checkpoints)). The template and this site use `CheckpointWriter`. Give checkpoints one owner: with Litestream attached, a checkpoint it did not make can remove WAL frames it has not copied yet.

The writer also runs `PRAGMA optimize` every six hours between batches (`writer.Options.OptimizeEvery`) and once more on `Close`.
