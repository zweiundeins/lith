# lith

[![CI](https://github.com/zweiundeins/lith/actions/workflows/ci.yml/badge.svg)](https://github.com/zweiundeins/lith/actions/workflows/ci.yml)
[![Go 1.27](https://img.shields.io/badge/go-1.27-00ADD8?logo=go&logoColor=white)](go.mod)
[![CGO_ENABLED=0](https://img.shields.io/badge/CGO__ENABLED-0-00ADD8?logo=go&logoColor=white)](#packages)
[![SQLite: one writer, WAL](https://img.shields.io/badge/SQLite-one%20writer%2C%20WAL-003B57?logo=sqlite&logoColor=white)](sqlite)
[![Datastar 1.0](https://img.shields.io/badge/Datastar-1.0-6c4fbb)](https://data-star.dev)
[![SSE: zstd or brotli](https://img.shields.io/badge/SSE-zstd%20%7C%20brotli-2e7d32)](sse)

**L**inear writes, **I**mmediate mode, **T**icks, **H**ypermedia: a runtime
for Go, SQLite and Datastar apps, built by [zwei und eins gmbh](https://zweiundeins.gmbh).

- **Linear writes:** one writer applies every command in order, batched into
  one transaction with a savepoint each.
- **Immediate mode:** a page is `view = f(state)`, rendered whole and morphed
  by Datastar; no client state predicts the server.
- **Ticks:** commits mark views dirty, and each changed view renders once per
  tick for everyone who watches it.
- **Hypermedia:** frames go out over SSE, each stream compressed on its own.

lith owns the runtime and nothing above it: routing stays with `net/http`,
HTML with templ, and each app keeps its own pages and domain model.

## Packages

| Package | What it does |
| --- | --- |
| [`sqlite`](sqlite) | one write connection and a `query_only` read pool whose connections keep their statements prepared, pragmas, `user_version` migrations, read-only opens, `VACUUM INTO` backups |
| [`writer`](writer) | the single writer: `Send`, `Do` and `DoValue`, batches with a savepoint per command, prepared statements, `Once` for retried requests, `Exclusive` for work between batches |
| [`events`](events) | event sourcing on the writer: typed streams, deciders that decide from a state row, projections that commit with their events, rebuilds into copies, catch-up projectors, upcasters, retention |
| [`live`](live) | topics mark views dirty; a tick feeds render workers that read one snapshot each and render each changed view once for all its viewers |
| [`sse`](sse) | Datastar events, formatted once and written to many streams, with zstd, brotli or gzip per stream and a deadline on every write; `ReadSignals` for any method; a memory budget that books what each stream holds, sizes its window from its frames, and can cap the streams by memory |
| [`edge`](edge) | sessions, CSRF, client IP, CSP with a nonce, compression, precompressed static files, rate limiters, health checks |
| [`tenant`](tenant) | one open database per account, refcounted, evicted at a soft cap |
| [`dbmetrics`](dbmetrics) | statement timing and a slow-query log through a wrapping driver |
| [`loadtest`](loadtest) | the writer and the render loop under load, in process, without HTTP |
| [`backup`](backup) | Litestream to S3: restore if missing, attach, snapshots, daily verify (its own module) |
| [`dbmetrics/prom`](dbmetrics/prom) | a Prometheus observer for `dbmetrics` (its own module) |
| [`prom`](prom) | the writer's, the hub's and the stream budget's numbers in Prometheus (its own module) |
| [`minify`](minify) | JavaScript and CSS minified with tdewolff/minify for `edge.Static`, before it hashes and precompresses them (its own module) |

Everything is pure Go and builds with `CGO_ENABLED=0`.

## Starting a project

```sh
go run github.com/zweiundeins/lith/cmd/lith-new@latest ~/develop/guestbook example.com/acme/guestbook
cd ~/develop/guestbook && go mod tidy && go tool task test
```

The template is a small guestbook that follows every rule once: a command, a
shared view, a per-tab view backed by SQLite, a button that stays pending until
its frame arrives. It comes with tests, CI, systemd and Caddy files, a deploy
script with rollback and a backup timer.

For work across checkouts, `lith-new -lith <dir>` writes a `replace`
directive to a local lith instead.

## Wiring

```go
db, err := sqlite.Open(ctx, "data/app.db", sqlite.Options{
	Migrations: migrations,
	Checkpoint: sqlite.CheckpointWriter,
})
hub := live.New(live.Options{Read: db.R})
w, err := writer.New(db, writer.Options{OnCommit: hub.Changed})

go w.Run(appCtx)
go hub.Run(hubCtx)
```

`synchronous` defaults to NORMAL. Beware: a power loss or kernel crash can then
undo up to about 35 seconds of acknowledged commits, those since the last
checkpoint, with Linux's default writeback. Set `Synchronous: "FULL"` where
that matters, at one fsync per batch.

A command reads and writes in one `Apply` and touches the topics it changes; a
handler sends it and answers 204; the page learns the outcome from its stream.
The recipes, the rules and a review checklist are in the skill.

## Measured

- **Writer:** on the 10-dollar VPS that serves the site (four vCPUs of an AMD
  EPYC), 80,530 commands a second at 200 producers with the default
  `synchronous=NORMAL`, 108 to a transaction; 14,480 with FULL, whose commits
  survive a power loss. On a development machine (i5-13500): 66,688 and 34,893
  ([bench/RESULTS.md](bench/RESULTS.md)). Beside 200 streams at 800 commands a
  second, a command commits in 0.3 to 0.5 ms at p50 with NORMAL and in 3 to
  11 ms with FULL there.
- **Streams:** each stream takes the smallest window that holds its frames.
  For a 230 KB page at 2^18: 845 KiB of heap with zstd (low memory) and 4,183
  bytes a frame; 1,603 KiB with brotli at quality 4 and 140 bytes a frame, at
  six times the CPU; 32 KiB a stream besides. 120 MiB holds 134 tabs of such a
  page with zstd ([bench/budget](bench/budget)).
- **Render loop:** 200 streams on one 139 KB view changing 800 times a second
  render once per tick and drop no frames ([loadtest](loadtest)).
- **In an app:** kanban on lith v0.2.0 commits about 14,500 moves a second at
  200 people, answers a move in 12.1 ms at p95, and with zstd needs half the
  server CPU it needs with brotli.

## The skill

[`skills/lith`](skills/lith) teaches an agent the rules, the recipes, the
numbers, a review checklist with `rg` patterns, and the Datastar gotchas the
projects learned. It ships with the code: point your agent at `skills/lith` of
the version you use, and its advice matches it.

## Credits

Inspired by [andersmurphy/hyperlith](https://github.com/andersmurphy/hyperlith) and
[zangster300/northstar](https://github.com/zangster300/northstar). The code started in
[kanban](https://github.com/zweiundeins/kanban),
[starbase](https://github.com/zweiundeins/starbase) and
[Libretto](https://libretto.ch).

## License

[MIT](LICENSE).

## Maintainer

Maintained by [zwei und eins gmbh](https://zweiundeins.gmbh), Aarberg, Switzerland.
