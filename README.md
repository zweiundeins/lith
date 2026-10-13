# lith

[![CI](https://github.com/zweiundeins/lith/actions/workflows/ci.yml/badge.svg)](https://github.com/zweiundeins/lith/actions/workflows/ci.yml)
[![Go 1.27](https://img.shields.io/badge/go-1.27-00ADD8?logo=go&logoColor=white)](go.mod)
[![CGO_ENABLED=0](https://img.shields.io/badge/CGO__ENABLED-0-00ADD8?logo=go&logoColor=white)](#packages)
[![SQLite: one writer, WAL](https://img.shields.io/badge/SQLite-one%20writer%2C%20WAL-003B57?logo=sqlite&logoColor=white)](sqlite)
[![Datastar 1.0](https://img.shields.io/badge/Datastar-1.0-6c4fbb)](https://data-star.dev)
[![SSE: zstd or brotli](https://img.shields.io/badge/SSE-zstd%20%7C%20brotli-2e7d32)](sse)

**L**inear writes, **I**mmediate mode, **T**icks, **H**ypermedia: the shared
runtime for the Go, SQLite and Datastar apps of zwei und eins.

- **Linear writes:** one writer applies every command in order, batched into
  one transaction with a savepoint each.
- **Immediate mode:** a page is `view = f(state)`, rendered whole and morphed
  by Datastar; no client state predicts the server.
- **Ticks:** commits mark views dirty, and each changed view renders once per
  tick for everyone who watches it.
- **Hypermedia:** frames go out over SSE, each stream compressed on its own.

The name nods to Anders Murphy's [hyperlith](https://github.com/andersmurphy/hyperlith),
whose design lith follows in Go. lith owns the runtime and nothing above it:
routing stays with `net/http`, HTML with templ, and each app keeps its own
pages and domain model.

## Packages

| Package | What it does |
| --- | --- |
| [`sqlite`](sqlite) | one write connection and a `query_only` read pool, pragmas, `user_version` migrations, read-only opens, `VACUUM INTO` backups |
| [`writer`](writer) | the single writer: `Send`, `Do` and `DoValue`, batches with a savepoint per command, prepared statements, `Once` for retried requests, `Exclusive` for work between batches |
| [`live`](live) | topics mark views dirty; a tick feeds render workers that read one snapshot each and render each changed view once for all its viewers |
| [`sse`](sse) | Datastar events, formatted once and written to many streams, with zstd, brotli or gzip per stream and a deadline on every write |
| [`edge`](edge) | sessions, CSRF, client IP, CSP with a nonce, compression, precompressed static files, rate limiters, health checks |
| [`tenant`](tenant) | one open database per account, refcounted, evicted at a soft cap |
| [`dbmetrics`](dbmetrics) | statement timing and a slow-query log through a wrapping driver |
| [`loadtest`](loadtest) | the writer and the render loop under load, in process, without HTTP |
| [`backup`](backup) | Litestream to S3: restore if missing, attach, snapshots, daily verify (its own module) |
| [`dbmetrics/prom`](dbmetrics/prom) | a Prometheus observer for `dbmetrics` (its own module) |

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

While this repository is private, Go needs to know: `go env -w
GOPRIVATE=github.com/zweiundeins/*`, plus git access to the repository. For
work across checkouts, `lith-new -lith <dir>` writes a `replace` directive
instead.

## Wiring

```go
db, err := sqlite.Open(ctx, "data/app.db", sqlite.Options{
	Synchronous: "FULL",
	Migrations:  migrations,
	Checkpoint:  sqlite.CheckpointWriter,
})
hub := live.New(live.Options{Read: db.R})
w, err := writer.New(db, writer.Options{OnCommit: hub.Changed})

go w.Run(appCtx)
go hub.Run(hubCtx)
```

A command reads and writes in one `Apply` and touches the topics it changes; a
handler sends it and answers 204; the page learns the outcome from its stream.
The recipes, the rules and a review checklist are in the skill.

## Measured

- **Writer:** 34,893 commands a second at 200 producers with
  `synchronous=FULL`, 122 to a transaction; 66,688 with NORMAL
  ([bench/RESULTS.md](bench/RESULTS.md)).
- **Streams:** 1.0 to 1.7 MiB of heap per open stream with zstd at a 2^19
  window (low-memory or full history), against 9.9 MiB for brotli at its
  default window, at the same compression.
- **Render loop:** 200 streams on one 139 KB view changing 800 times a second
  render once per tick and drop no frames ([loadtest](loadtest)).
- **In an app:** kanban on lith commits 14,928 moves a second at 200 people,
  answers a move in 13.1 ms at p95, and with zstd needs half the server CPU it
  needed with brotli.

## The skill

[`skills/lith`](skills/lith) teaches an agent the rules, the recipes, the
numbers, a review checklist with `rg` patterns, and the Datastar gotchas the
projects learned. It ships with the code, so its advice matches the version in
use. Install it in every Claude Code root with a line in claude-config's
`skills.txt`:

```text
lith	https://github.com/zweiundeins/lith	skills/lith
```

## Credits

The design follows Anders Murphy's [hyperlith](https://github.com/andersmurphy/hyperlith)
and his writing at [andersmurphy.com](https://andersmurphy.com). The code
started in kanban, starbase and libretto, and the template's layout after
[northstar](https://github.com/zangster300/northstar).
