---
title: Reads
summary: Every read runs in a read transaction of the pool, the one it is given: a document's, a render's or a command's. Query code takes a sqlite.Querier, so one function serves all three.
order: 3
---

`sqlite.Open` opens two pools on one file: `db.W`, a single connection that belongs to the writer and the migrations, and `db.R`, a pool of read connections with `PRAGMA query_only` set, which refuse any write. Everything that reads goes through `db.R`, in a read transaction.

## One transaction for a document

A GET renders its whole document from one read transaction, so every part of the page shows the same moment. `db.View(ctx, fn)` begins it, runs `fn` and ends it:

```go source=site/internal/docs/example/web/server.go
ctx := r.Context()
var buf bytes.Buffer
err = s.db.View(ctx, func(tx *sql.Tx) error {
	b, err := board.Load(ctx, tx, id)
	if err != nil {
		return err
	}
	prefs, err := board.SessionPrefs(ctx, tx, edge.SessionID(ctx))
	if err != nil {
		return err
	}
	lanes, err := renderLanes(ctx, b)
	if err != nil {
		return err
	}
	return Page(PageData{Tab: newID(), Datastar: s.datastar, Board: b, Lanes: lanes, Prefs: prefs}).Render(ctx, &buf)
})
```

The handler renders into a buffer inside the transaction and writes the buffer after it, as the template's does: an error halfway through still gets a clean 500, and the transaction never waits for a slow client.

A GET writes nothing: no session row, no visit counter, no "last seen". A crawler multiplies every write a GET makes, and a write would need the writer anyway. The tab's row appears with its first action.

`db.ViewValue` is `View` for a function that returns a value. It returns the zero value with any error:

```go source=site/internal/docs/example/web/server.go
state, err := s.db.ViewValue(ctx, func(tx *sql.Tx) (board.TabState, error) { return board.Tab(ctx, tx, sid, tab) })
```

A read transaction's snapshot begins with its first read: until then, a commit can still land in it.

## Query code that every reader shares

A query function takes a `sqlite.Querier`, the methods `*sql.DB`, `*sql.Tx` and `*sql.Conn` share. A command's `*writer.Tx` has them too, and so has the querier of a render. One function then serves a GET, a view and a command:

```go source=site/internal/docs/example/board/queries.go
// LaneCount is the number of cards on a lane. It takes a sqlite.Querier,
// so a GET, a view and a command all call it: a *sql.Tx, a render's
// sqlite.QuerierFrom(ctx) and a *writer.Tx are each one.
func LaneCount(ctx context.Context, q sqlite.Querier, lane int64) (n int, err error) {
	err = q.QueryRowContext(ctx, `SELECT count(*) FROM cards WHERE lane_id = ? AND archived IS NULL`, lane).Scan(&n)
	return n, err
}
```

In a command, the `*writer.Tx` is the querier, and its context is the command's (`tx.Context()`):

```go source=site/internal/docs/example/board/commands.go
n, err := LaneCount(tx.Context(), tx, c.Lane) // the same query a GET and a view run
```

A `*writer.Tx` runs the query in the batch's transaction, on the writer's statement cache, and ignores the context it is given: a statement never stops halfway through a batch.

## Reads in a view

A view's `Render` gets the worker's read transaction. `sqlite.QuerierFrom(ctx)` returns a querier for it that runs each SQL text on a statement the worker prepared once, on its own connection:

```go source=site/internal/docs/example/web/views.go
Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
	q, _ := sqlite.QuerierFrom(ctx) // the worker's prepared statements, in this render's transaction
	b, err := board.Load(ctx, q, id)
	if err != nil {
		return nil, err
	}
	return renderBoard(ctx, b)
},
```

modernc's SQLite parses and plans every query that has no prepared statement, so a render that queries through `tx` directly pays that each time. Each worker keeps the 512 statements it used last. SQL built at runtime, such as an `IN` list with a mark per value, belongs on `tx`, which keeps nothing.

As with a command, scan a `QueryRow` before the same query runs again, and close rows before the render returns: a statement with an unscanned row or open rows is busy.

## Code that takes only a context

Some code reads through a store with a context instead of a transaction. `db.View(ctx, fn)` recognizes a context that carries a transaction on the same database file and runs `fn` in it, so the store reads the caller's snapshot:

```go source=site/internal/docs/example/board/store.go
// Board reads a board. Called from a live Render, or with a ctx that
// sqlite.WithTx made, it reads in that transaction, so it sees the
// caller's snapshot; with any other ctx it opens a read transaction of its
// own.
func (s Store) Board(ctx context.Context, id int64) (Board, error) {
	return s.DB.ViewValue(ctx, func(tx *sql.Tx) (Board, error) { return Load(ctx, tx, id) })
}
```

A render's context carries its worker's transaction already. A GET hands its own down with `sqlite.WithTx`:

```go source=site/internal/docs/example/board/board_test.go
ctx := sqlite.WithTx(ctx, tx) // code that takes only a ctx joins this snapshot
b, err := store.Board(ctx, id)
```

The transaction's owner ends it; a `View` that joined one leaves it open. `sqlite.TxFrom(ctx)` returns the transaction a context carries. A context that carries a transaction on another file, as when a render of one database reads another, gets a transaction of its own ([One database per account](/docs/guides/tenants#views-of-an-account)).

## The read pool

`sqlite.Options` sizes the pool and its connections:

| Option | Default | What it does |
| --- | --- | --- |
| `Readers` | GOMAXPROCS + 4, at least 8 | connections in `db.R` |
| `CacheKiB` | 32000 | each connection's page cache, in KiB |
| `MmapBytes` | 256 MiB | how much of the file each reader maps; negative for none |
| `BusyTimeout` | 5 s | how long a connection waits for a lock |

The hub's render workers hold one read connection each for the hub's life, GOMAXPROCS of them by default, and everything else shares the rest: the default leaves four or more for GET documents, jobs and probes. Size `Readers` as the hub's `Workers` plus the reads that run at once beside it. When the pool has no more connections than the hub has workers, the hub logs a warning and runs one worker fewer than the pool has connections.

The page cache is per connection: eight connections at the default may hold about 250 MB between them on a large database. This site runs two workers, six readers and 4,096 KiB a connection, for its small tables ([its unit's memory settings](/docs/guides/deploying#memory)).

## Reading a copy, or from a tool

A CLI that only reads, or a check of a backup, opens the file with `ReadOnly`. It opens no write connection and applies no migrations, and a missing file is an error instead of a new, empty database:

```go source=site/internal/docs/example/board/store.go
func Check(ctx context.Context, path string) error {
	db, err := sqlite.Open(ctx, path, sqlite.Options{ReadOnly: true})
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.R.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("%s: %s", path, result)
	}
	return nil
}
```
