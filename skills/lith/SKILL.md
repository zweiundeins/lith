---
name: lith
description: How zwei und eins builds Go + SQLite + Datastar apps on the shared module github.com/zweiundeins/lith, short for Linear writes, Immediate mode, Ticks, Hypermedia (one batching writer, a read pool, a tick-based render loop with shared views, per-stream zstd or brotli SSE, the request edge). Use when writing, reviewing or debugging code in a project that imports lith (starbase, kanban, 2und1.ch-2027, libretto, swisscyberguard, pendlerpreis), when moving a project onto lith, or when starting a new app of that kind. Covers commands, views and topics, tab state, migrations, backups, load tests, and the Datastar gotchas these projects share.
---

# lith

The Go projects of zwei und eins share one architecture: CQRS on one SQLite
file, server-rendered views pushed over SSE, Datastar in the browser. lith is
that runtime as a module, and its name is its design: Linear writes, Immediate
mode, Ticks, Hypermedia. This skill ships in the lith repo, so its rules match
the lith version in a project's go.mod.

The reasoning behind the design is Anders Murphy's (hyperlith,
andersmurphy.com); the `andersmurphy` skill has his posts. kanban's
`benchmarks/RESULTS.md` has the numbers measured in Go.

## The rules

1. **Only the writer writes.** Every state change is a command:
   `Apply(tx *writer.Tx) error`, or a `writer.Func`. Handlers validate, call
   `w.Send(cmd)` (no wait) or `w.Do(ctx, cmd)` (waits for the commit) and
   answer 204. The writer drains the queue into one `BEGIN IMMEDIATE`
   transaction with a savepoint per command, in the order the commands
   arrived; `w.Flush(ctx)` returns once everything queued before it is
   applied. Checks that read before they write go inside the command.
   Commands never render and never call the network; `tx.AfterCommit` runs
   what must follow a commit. A failed Send goes to `OnSendError`, else to
   the log.
2. **The writer outlives the HTTP server.** Ending `w.Run`'s context drains
   the queue like `w.Close()`, and from then on Send and Do return
   `ErrClosed`. Run it on a context that outlives the server, never the
   signal context, and call `w.Close()` after `srv.Shutdown`, so actions
   still write while the server drains.
3. **A command touches the topics it changes** (`tx.Touch("board:42")`).
   Views listen to topics. A command that touches nothing leaves pages stale.
4. **Views are `view = f(state)`.** A page region has one render function. A
   view has a key (everyone who sees the same thing shares it) and topics.
   The render loop renders each changed view once per tick, inside one read
   transaction per worker, and every stream on it gets the same frame.
   Per-user parts go in a small view of their own (`tab:<id>`) or a signal,
   never into a shared view.
5. **The outcome travels on the stream.** An action's response carries
   nothing the view needs. No optimistic updates: the page shows the user's
   own input at once and marks it pending; outcomes appear when the frame
   brings them; a refusal says why, next to where the user acted.
6. **State that must survive a deploy lives in SQLite**, written by
   commands: tab state per tab id, preferences per session. Ephemeral client
   state lives in `_`-prefixed signals. No maps of tabs in memory.
7. **GET never writes.** Sessions start as a cookie (`edge`); a row appears
   with the first command that needs one.
8. **Reads go through the transaction you are given**: the worker's in a
   view, `db.View` for a GET document. A render's ctx carries its
   transaction, so a store that calls `db.View(ctx, fn)` from a render joins
   the render's snapshot. Query code takes a `sqlite.Querier`: `*sql.DB`,
   `*sql.Tx`, `*writer.Tx` and a render's `sqlite.QuerierFrom(ctx)` are all
   one, so one function serves a GET, a view and a command. Scan every
   `QueryRow` before the same query runs again: a command's Tx and a
   render's Querier run each SQL text on one cached statement, and an
   unscanned row keeps it busy. `db.R` is `query_only`; `db.W` belongs to
   the writer and migrations.
9. **Every stream is bounded**: a cap on open streams, a deadline on every
   network write, the newest frame wins, shutdown cancels streams.
10. **Keys are rowids or time-ordered.** Random tokens go on small tables; a
    bulk insert sorts its batch by key first.
11. **Measure.** Writer `Stats()`, the hub's `Stats()` (read transactions,
    renders by view key), the tick overrun log, `lith/loadtest`. Numbers go
    into `benchmarks/RESULTS.md` with date and machine.

## Packages

| Package | Holds |
| --- | --- |
| `sqlite` | `Open`: one write connection and a `query_only` read pool (or `ReadOnly`) whose connections keep their statements prepared (`Statements`), pragmas, `user_version` migrations or the project's own runner (`Migrate`), `View` that joins a ctx's transaction (`WithTx`) and `ViewValue` that returns a result, `Querier`, `Backup` (VACUUM INTO), a wrapping `Driver` |
| `writer` | the single writer: `Send`, `Do`, `DoValue` (a command's result, typed), `Flush`, batches with savepoints, statement cache, `Once` and `OnceResult` for op ids, `OnCommit` topics, `OnSendError`, `Exclusive` for what a transaction cannot hold, optimize and checkpoints |
| `live` | views and topics, the tick, render workers with prepared read statements, shared frames, `Watch` before a stream opens, `WriteTick`, stream cap, presence, a heartbeat signal, stats |
| `sse` | Datastar event formatting, per-stream zstd (level and window), brotli or gzip, write deadlines |
| `edge` | sessions cookie, CSRF, client IP, CSP nonce, compression, precompressed static files, rate limiters (bucket, fixed or sliding window), health |
| `backup` | Litestream: restore if missing, attach, snapshots, daily verify (nested module) |
| `prom` | Prometheus collector for `writer.Stats`, `live.Hub.Stats` and the stream budget: `<ns>_writer_*`, `<ns>_live_*`, `<ns>_sse_budget_*` (nested module) |
| `tenant` | one database file per account: refcounted pool with LRU eviction |
| `dbmetrics` | query timing and slow-query log |
| `loadtest` | writer and render loop under load, in process |

## Where to look next

- `references/recipes.md`: wiring and migrations, a command, op ids that
  keep refusals, query code shared by reads and commands, a view with topics,
  tab state and preferences, a background job, a bulk import, work a
  transaction cannot hold, an event-sourced command, backups, a new project.
- `references/performance.md`: measured numbers, the knobs they set, how to
  measure.
- `references/review.md`: the checklist with `rg` patterns.
- `references/datastar.md`: the Datastar gotchas the projects learned.

A project's own CLAUDE.md or AGENTS.md says what is specific to it (Rocket
components in starbase, tenancy and billing in libretto) and which lith
version it uses.
