---
name: lith
description: How zwei und eins builds Go + SQLite + Datastar apps on the shared module github.com/zweiundeins/lith (one batching writer, a read pool, a tick-based render loop with shared views, per-stream zstd or brotli SSE, the request edge). Use when writing, reviewing or debugging code in a project that imports lith (starbase, kanban, 2und1.ch-2027, libretto, swisscyberguard, pendlerpreis), when moving a project onto lith, or when starting a new app of that kind. Covers commands, views and topics, tab state, migrations, backups, load tests, and the Datastar gotchas these projects share.
---

# lith

The Go projects of zwei und eins share one architecture: CQRS on one SQLite
file, server-rendered views pushed over SSE, Datastar in the browser. lith is
that runtime as a module. This skill ships in the lith repo, so its rules match
the lith version in a project's go.mod.

The reasoning behind the design is Anders Murphy's (hyperlith,
andersmurphy.com); the `andersmurphy` skill has his posts. kanban's
`benchmarks/RESULTS.md` has the numbers measured in Go.

## The rules

1. **Only the writer writes.** Every state change is a command:
   `Apply(tx *writer.Tx) error`, or a `writer.Func`. Handlers validate, call
   `w.Send(cmd)` (no wait) or `w.Do(ctx, cmd)` (waits for the commit) and
   answer 204. The writer drains the queue into one `BEGIN IMMEDIATE`
   transaction with a savepoint per command. Checks that read before they
   write go inside the command. Commands never render and never call the
   network; `tx.AfterCommit` runs what must follow a commit.
2. **A command touches the topics it changes** (`tx.Touch("board:42")`).
   Views listen to topics. A command that touches nothing leaves pages stale.
3. **Views are `view = f(state)`.** A page region has one render function. A
   view has a key (everyone who sees the same thing shares it) and topics.
   The render loop renders each changed view once per tick, inside one read
   transaction per worker, and every stream on it gets the same frame.
   Per-user parts go in a small view of their own (`tab:<id>`) or a signal,
   never into a shared view.
4. **The outcome travels on the stream.** An action's response carries
   nothing the view needs. No optimistic updates: the page shows the user's
   own input at once and marks it pending; outcomes appear when the frame
   brings them; a refusal says why, next to where the user acted.
5. **State that must survive a deploy lives in SQLite**, written by
   commands: tab state per tab id, preferences per session. Ephemeral client
   state lives in `_`-prefixed signals. No maps of tabs in memory.
6. **GET never writes.** Sessions start as a cookie (`edge`); a row appears
   with the first command that needs one.
7. **Reads go through the transaction you are given**: the worker's in a
   view, `db.View` for a GET document. `db.R` is `query_only`; `db.W` belongs
   to the writer and migrations.
8. **Every stream is bounded**: a cap on open streams, a deadline on every
   network write, the newest frame wins, shutdown cancels streams.
9. **Keys are rowids or time-ordered.** Random tokens go on small tables; a
   bulk insert sorts its batch by key first.
10. **Measure.** Writer `Stats()`, the tick overrun log, `lith/loadtest`.
    Numbers go into `benchmarks/RESULTS.md` with date and machine.

## Packages

| Package | Holds |
| --- | --- |
| `sqlite` | `Open`: one write connection and a `query_only` read pool, pragmas, `user_version` migrations, `View`, `Backup` (VACUUM INTO) |
| `writer` | the single writer: `Send`, `Do`, batches with savepoints, statement cache, `Once` for op ids, `OnCommit` topics, optimize and checkpoints |
| `live` | views and topics, the tick, render workers, shared frames, stream cap, presence, overrun stats |
| `sse` | Datastar event formatting, per-stream zstd, brotli or gzip, write deadlines |
| `edge` | sessions cookie, CSRF, client IP, CSP nonce, compression, precompressed static files, rate limiter, health |
| `backup` | Litestream: restore if missing, attach, snapshots, daily verify (nested module) |
| `tenant` | one database file per account: refcounted pool with LRU eviction |
| `dbmetrics` | query timing and slow-query log |
| `loadtest` | writer and render loop under load, in process |

## Where to look next

- `references/recipes.md`: a command, a view with topics, tab state and
  preferences, a background job, a bulk import, an event-sourced command,
  backups, a new project.
- `references/performance.md`: measured numbers, the knobs they set, how to
  measure.
- `references/review.md`: the checklist with `rg` patterns.
- `references/datastar.md`: the Datastar gotchas the projects learned.

A project's own CLAUDE.md or AGENTS.md says what is specific to it (Rocket
components in starbase, tenancy and billing in libretto) and which lith
version it uses.
