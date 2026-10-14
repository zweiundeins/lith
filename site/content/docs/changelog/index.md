---
title: Changelog
headline: Changelog: what each lith release changed
summary: What each release of lith changed.
description: What each release of lith changed, from v0.1.0 on: new packages and functions, changed behaviour, and what an app has to change to upgrade.
order: 9
---

Until 1.0, a minor release may change the API; its entry says what to change in an app.

## Unreleased

- `sse`: `ReadSignals` reads a request's Datastar signals for any method: the `datastar` query parameter of GET, HEAD and DELETE, the JSON body of the others, `@query`'s QUERY included. It caps the body at 1 MiB (`ReadSignalsLimit` for another cap), takes a request without signals as none, and reads the body to its end, so that over HTTP/1.1 a handler that waits before it answers still notices a client that left. Called after `Open`, it returns `ErrBodyConsumed`.
- `edge`: `Compress` compresses the answers to QUERY as it does GET's.
- `sse`: a brotli stream that opens under memory pressure now takes a smaller tier instead of going out uncompressed: its quality's windows down to `MinWindow`, then quality 3 at smaller windows down to 2^16 (912 KiB). A brotli stream keeps its tier until it closes, so past 20 open streams it takes at most its largest tier times twice the share of the limit that is left: a budget of 150 MiB compresses 93 brotli streams instead of 57, and the first 20 still get the largest tier. A brotli stream goes out uncompressed only when even 2^16 does not fit, and `Refuse` turns away only such a stream. The tier's quality now applies to the encoder, and `BudgetStats` names it (`br q3 2^16`).

## v0.1.1 (2026-10-14)

- `sse`: with `ZstdLowMemory` set and a memory budget (every `live` hub has one), a client that accepts brotli but not zstd, such as Safari, got its stream uncompressed: the low-memory filter dropped the brotli tiers too. It now narrows zstd's tiers only. Apps that set `ZstdLowMemory` should update.

## v0.1.0 (2026-10-13)

The first release: the runtime for Go, SQLite and Datastar apps, with a template for new apps, a skill for coding agents and the website at lith.zweiundeins.gmbh. It needs Go 1.27 and builds with `CGO_ENABLED=0`.

### Packages

- `sqlite`: one write connection and a `query_only` read pool; pragmas; migrations by `user_version` or a project's own runner; read-only opens; `View` and `ViewValue` in one snapshot, joining a transaction a ctx carries; `VACUUM INTO` backups. Each read connection keeps the 128 statements it ran last prepared.
- `writer`: the single writer. `Send`, `Do` and `DoValue` queue commands, which run in batches of one transaction with a savepoint each, on statements prepared once; `Once` and `OnceResult` for requests a client retries; `Flush`; `Exclusive` for work between batches; checkpoints and `PRAGMA optimize`.
- `live`: commits touch topics, topics mark views dirty, and each tick renders every dirty view once, in one read transaction per worker, for all the streams that watch it. Presence, `Every`, `Watch` and `Unwatch`, `WriteTick`, a stream cap, heartbeats, and `Spec.Reconnect`, which ends each stream with `sse.Reconnect` when the server stops, so that pages open their stream again on the next process.
- `sse`: Datastar's events, formatted once and written to many streams; zstd, brotli or gzip per stream, with a deadline on every write; a memory budget that shrinks the streams' windows as more open; idle streams that give their encoder back (`Hibernate`).
- `edge`: sessions, CSRF, the client's address behind a proxy, a CSP with a nonce, compression, precompressed static files, rate limiters, request logs, health checks.
- `tenant`: one open database per account, refcounted, evicted at a soft cap.
- `dbmetrics`: statement timing and a slow-query log through a wrapping driver.
- `loadtest`: the writer and the render loop under load, in process, without HTTP.
- `backup` (a module of its own): Litestream to S3, a restore when the file is missing, snapshots, a daily verify.
- `dbmetrics/prom` and `prom` (modules of their own): the statements', the writer's, the hub's and the stream budget's numbers in Prometheus.

### Defaults to know

- `synchronous=NORMAL`: a power loss or a kernel crash can undo the commits of the last 35 seconds or so; a crash of the process loses none. `sqlite.Options{Synchronous: "FULL"}` syncs every batch.
- The streams share a memory budget of half of GOMEMLIMIT, or of the cgroup's limit: a few streams compress with 512 KiB windows, many with 256 KiB, and the rest go out uncompressed, or not at all with `Refuse`.
- An idle stream keeps its encoder until `live.Options.Hibernate` is set.
- A page opens its stream with `retry: 'error'` from an element with an id, the one `Spec.Reconnect` names.

### Tools

- `cmd/lith-new` copies the template into a new module that requires the lith it ran from: a guestbook with tests, CI, a systemd unit, a Caddy block, a deploy script with rollback and a backup timer.
- `skills/lith`: the rules for coding agents, with review commands, Datastar notes, recipes and performance numbers.
- `bench`: the writer, render and stream benchmarks, with the commands to run them again (`bench/RESULTS.md`).

### Website

lith.zweiundeins.gmbh runs on lith, from `site/`: the landing page, the docs (guides that follow one app, Datastar notes, performance, the package reference from the doc comments, a review checklist and this changelog), and eight live demos: a guestbook, a writer race, a stream inspector, a pixel canvas, a shared board, a cursor party, a Game of Life and a million checkboxes.
