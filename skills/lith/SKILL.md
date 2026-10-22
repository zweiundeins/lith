---
name: lith
description: How to build Go + SQLite + Datastar apps on github.com/zweiundeins/lith, short for Linear writes, Immediate mode, Ticks, Hypermedia (one batching writer, a read pool, a tick-based render loop with shared views, per-stream zstd or brotli SSE, the request edge). Use when writing, reviewing or debugging code in a project that imports lith, when moving a project onto lith, or when starting a new app of that kind. Covers commands, views and topics, tab state, migrations, backups, load tests, and the Datastar gotchas such apps run into.
---

# lith

lith is a runtime for Go apps of one architecture, built by zwei und eins gmbh:
CQRS on one SQLite file, server-rendered views pushed over SSE, Datastar in
the browser. Its name is its design: Linear writes, Immediate
mode, Ticks, Hypermedia. This skill ships in the lith repo, so its rules match
the lith version in a project's go.mod.

The reasoning behind the design is Anders Murphy's (hyperlith,
andersmurphy.com). The kanban example app (github.com/zweiundeins/kanban)
has the numbers measured in Go in `benchmarks/RESULTS.md`.

## The rules

1. **Only the writer writes.** Every state change is a command:
   `Apply(tx *writer.Tx) error`, or a `writer.Func`. Handlers validate, call
   `w.Do(ctx, cmd)` (waits for the commit: for what a person asked for) or
   `w.Send(cmd)` (no wait: only for updates the next one replaces) and
   answer 204. The writer drains the queue into one `BEGIN IMMEDIATE`
   transaction with a savepoint per command, in the order the commands
   arrived; `w.Flush(ctx)` returns once everything queued before it is
   applied. Checks that read before they write go inside the command.
   Commands never render and never call the network; `tx.AfterCommit` runs
   what must follow a commit. `tx.Publish(v)` hands a value to in-process
   listeners (`w.Subscribe[T]`) after the commit, best effort: never for
   pages (views and `Touch` cover them), and a listener that must not miss
   a value reconciles from the data. A failed Send goes to `OnSendError`,
   else to the log.
2. **The writer outlives the HTTP server.** Ending `w.Run`'s context drains
   the queue like `w.Close()`, and from then on Send and Do return
   `ErrClosed`. Run it on a context that outlives the server, never the
   signal context, and call `w.Close()` after `srv.Shutdown`, so actions
   still write while the server drains.
3. **A command touches the topics it changes** (`tx.Touch("board:42")`).
   Views listen to topics. A command that touches nothing leaves pages stale.
   Topics are paths: `board:42` also counts for `board`, and `src:A:*`
   touches everything below `src:A`. Ids from outside go through
   `live.Topic(parts...)`.
4. **Views are `view = f(state)`.** A page region has one render function. A
   view has a key (everyone who sees the same thing shares it) and topics.
   The render loop renders each changed view once per tick, inside one read
   transaction per worker, and every stream on it gets the same frame.
   Per-user parts go in a small view of their own (`tab:<id>`) or a signal,
   never into a shared view. A view whose reads depend on a row, such as a
   tab's route, declares what it read with `live.Depends(ctx, topics...)`
   during Render instead of changing its key.
5. **The outcome travels on the stream.** An action's response carries
   nothing the view needs. No optimistic updates: the page shows the user's
   own input at once and marks it pending; outcomes appear when the frame
   brings them; a refusal says why, next to where the user acted. A status
   code carries nothing a person reads: what the person caused (a
   validation error, a refusal, a quota, a card that is gone) is an outcome
   in the tab's row, answered 204; what no command decided (the writer
   failed, an abuse-sized rate limit, the server stopping) is a notice
   (`hub.Notify`, `Options.Stopping`); both carry the op id that ends the
   pending state. The page uses its own words only for what the browser
   alone knows: `retries-failed`, and a stream that connects again with a
   tab row that does not ack the op, once no request of it is in flight.
   Never a clock: a command that runs long records its acceptance first,
   which acks the op at once. Stream IDs name the session and the tab.
6. **State that must survive a deploy lives in SQLite**, written by
   commands: tab state per tab id, preferences per session. Ephemeral client
   state lives in `_`-prefixed signals. No maps of tabs in memory.
7. **GET never writes.** Sessions start as a cookie (`edge`); a row appears
   with the first command that needs one. A session id is an identifier, not
   a limiter key: a client mints a new one with every request that sends no
   cookie, so limit by `edge.ClientKey(r)` first. Static files are mounted
   outside `edge.Sessions`: a cookie on a public, immutable answer goes to
   everyone a shared cache hands it to. `@get` for streams and reads whose
   signals are all `_`; `@query` for reads that send real signals (searches,
   filters), with `openWhenHidden: false` when it opens a stream; `@post` for
   commands. Read signals with `sse.ReadSignals` for any method, before
   `sse.Open`: it caps the body and gives it 10 s (`sse.SignalsTimeout`).
8. **Reads go through the transaction you are given**: the worker's in a
   view, `db.View` for a GET document. A render's ctx carries its
   transaction, so a store that calls `db.View(ctx, fn)` from a render joins
   the render's snapshot. Query code takes a `sqlite.Querier`: `*sql.DB`,
   `*sql.Tx`, `*writer.Tx` and a render's `sqlite.QuerierFrom(ctx)` are all
   one, so one function serves a GET, a view and a command. Scan every
   `QueryRow` before the same query runs again: a command's Tx and a
   render's Querier run each SQL text on one cached statement, and an
   unscanned row keeps it busy. SQL built at runtime runs with
   `sqlite.Uncached(ctx)`, or `tx.QueryUncached` in a command. `db.R` opens
   the file read-only (`mode=ro`) and is `query_only`, but can still ATTACH
   another file, so SQL texts come from constants; `db.W` belongs to the
   writer and migrations.
9. **Every stream is bounded**: a cap on open streams, and one per client
   (`Spec.Client: edge.ClientKey(r)`, `Options.MaxStreamsPerClient`, 16), a
   memory budget (set GOMEMLIMIT or a `Limit`), a deadline on every network
   write, the newest frame wins (in the page too, with `newest-frame.js`),
   shutdown cancels streams. An action that stores tab state refuses a tab
   without an open stream (`Hub.HasStream`).
10. **Keys are rowids or time-ordered.** Random tokens go on small tables; a
    bulk insert sorts its batch by key first.
11. **Measure.** Writer `Stats()`, the hub's `Stats()` (read transactions,
    renders by view key), the tick overrun log, `lith/loadtest`. Numbers go
    into `benchmarks/RESULTS.md` with date and machine.

## Packages

| Package | Holds |
| --- | --- |
| `sqlite` | `Open`: one write connection and a read pool opened read-only and `query_only` (or `ReadOnly`) whose connections keep their statements prepared (`Statements`), pragmas, `user_version` migrations or the project's own runner (`Migrate`), `View` that joins a ctx's transaction (`WithTx`) and `ViewValue` that returns a result, `Querier`, `Uncached` for SQL built at runtime, `Backup` (VACUUM INTO), a wrapping `Driver` |
| `writer` | the single writer: `Send`, `Do`, `DoValue` (a command's result, typed), `Flush`, batches with savepoints, statement cache, `Once` and `OnceResult` for op ids, `OnCommit` topics, `Publish` and `Subscribe` for in-process listeners, `OnSendError`, `Exclusive` for what a transaction cannot hold, optimize and checkpoints |
| `events` | event sourcing on the writer: typed streams, deciders that decide from a state row (`Execute`, expected versions), `Append`, inline projectors in the same savepoint, catch-up projectors after the commit, a `Runner` that rebuilds changed projectors into copies and swaps them in, upcasters, retention, `eventstest` |
| `live` | views and topics, a topic hierarchy (`board:42` is also `board`, `src:A:*` everything below `src:A`, `Topic` and `Below`), topics a Render declares (`Depends`), commit numbers and topic versions for caches (`Hub.Seq`, `Hub.Version`, `Seq(ctx)`), the tick, render workers with prepared read statements, shared frames, no render while every watcher has a frame waiting, `Watch` before a stream opens, notices sent once to one stream (`Notify`) and when the hub stops (`Options.Stopping`), `WriteTick`, stream caps, overall and per client (`Spec.Client`, `HasStream`), presence, a heartbeat signal, fewer frames for slow links (`Spec.Slow`), stats, `newest-frame.js` for the page (`NewestFrame`, `Scripts`) and a count of pages behind their stream (`Behind`), pacing a page that falls behind (`Spec.Ack`, `Hub.Pace`, `Hub.Ack`, `Spec.Release`) |
| `sse` | Datastar event formatting, per-stream zstd (level and window), brotli or gzip, a memory budget (a quarter of the machine's memory without a memory limit), write deadlines, `ReadSignals` for any method, with a deadline on the body |
| `edge` | sessions cookie, CSRF, client IP (`TrustedProxies` for the log line at start), slow links (`SlowLink`, `NetworkHints`), CSP nonce, compression (`Cache-Control: no-transform` opts a page out: one with a user's secret beside reflected input, against BREACH), precompressed static files (minified with `Minify`, `Warmed` for readiness), rate limiters (bucket, fixed or sliding window), health |
| `backup` | Litestream: restore if missing, attach, snapshots, daily verify (nested module) |
| `prom` | Prometheus collector for `writer.Stats`, `live.Hub.Stats`, the stream budget and the event store's and runner's stats: `<ns>_writer_*`, `<ns>_live_*`, `<ns>_sse_budget_*`, `<ns>_events_*` (nested module) |
| `minify` | `minify.Bytes` for `edge.Minify`: JavaScript and CSS minified with tdewolff/minify before `edge.Static` hashes and precompresses them (nested module) |
| `tenant` | one database file per account: refcounted pool with LRU eviction |
| `dbmetrics` | query timing and slow-query log |
| `loadtest` | writer and render loop under load, in process |

## Where to look next

- `references/recipes.md`: wiring and migrations, a command, op ids that
  keep refusals, in-process listeners, query code shared by reads and
  commands, a view with topics, tab state and preferences, a per-tab view
  that navigates, caching an aggregate against a version, a background job, a bulk import, work a
  transaction cannot hold, an event-sourced command, projections of events
  and their rebuilds, backups, monitoring on a shared host, a new project.
- `references/performance.md`: measured numbers, the knobs they set, how to
  measure.
- `references/review.md`: the checklist with `rg` patterns.
- `references/datastar.md`: the Datastar gotchas apps on lith ran into.

A project's own CLAUDE.md or AGENTS.md says what is specific to it and which
lith version it uses.
