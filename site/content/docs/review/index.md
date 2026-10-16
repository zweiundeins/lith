---
title: Review checklist
headline: Review checklist for lith apps, with rg commands
summary: What to check in a lith app before a release, and on every change to the database, a stream or a handler. Each check names the mistake, what it does in production, and an rg command that finds it.
description: What to check in a lith app before a release and on every change: each check names the mistake, what it does in production, and an rg command to find it.
order: 7
---

The lith projects made each of these mistakes, and most were found by reading code with a search. Run the commands from the project's root; each match is a place to look at. The checks match the review checklist in lith's skill, which coding agents use; [the skill and this page](#the-skill-and-this-page) say how the two stay together.

## Writes

### A write outside the writer

Every `Exec` must sit inside a command (an `Apply(tx *writer.Tx)` or a `writer.Func`), an `Exclusive` function, a migration, or a test. A store method that writes on its own is a second writer: it competes with the writer for SQLite's one write lock, and under load one of them fails with `SQLITE_BUSY`.

```sh
rg -n '\.(Exec|ExecContext)\(' --type go -g '!*_test.go'
```

### A second connection that writes

A second connection to the database file that writes, for a factory reset or an import, takes the write lock beside the writer. A transaction on it that runs past the busy timeout fails the writer's batches with `SQLITE_BUSY`. Pragma toggles, `ATTACH`, `DETACH` and `VACUUM` belong in `w.Exclusive`, on the writer's own connection ([Migrations, jobs and imports](/docs/guides/migrations-and-jobs#work-a-transaction-cannot-hold)). Outside lith itself, look at every match:

```sh
rg -n 'sql\.Open\(' --type go
```

### A deferred transaction that reads, then writes

A transaction begun without `IMMEDIATE` takes a read snapshot first and the write lock later. When another connection, Litestream's say, commits in between, the upgrade fails with `SQLITE_BUSY_SNAPSHOT`, and `busy_timeout` does not retry it.

```sh
rg -n 'BeginTx\([^)]*nil\)|\.Begin\(\)' --type go
```

On lith's write connection every transaction is `IMMEDIATE`; a match elsewhere needs a look.

### One connection instead of a writer

A pool pinned to one connection serialises the writes but does not batch them: every write pays its own commit, and reads queue behind the writes. Outside lith:

```sh
rg -n 'SetMaxOpenConns\(1\)' --type go
```

### A change made of several autocommits

A change that runs as several statements outside one transaction, an insert, then an audit row, then a publish, is not atomic: a crash between two leaves half of it. One command, one `Apply`, one savepoint.

### SQL built at runtime in a command

A command that builds its SQL at runtime, such as an `IN` list with a mark per value, takes a place in the writer's statement cache for every text it makes, and pushes out the statements that repeat. Run it through `tx.ExecUncached`, `tx.QueryUncached` or `tx.QueryRowUncached`, or pass the list as one JSON argument ([Commands](/docs/guides/commands#statements)).

```sh
rg -n 'tx\.(Exec|Query|QueryRow)\((fmt\.Sprintf|"[^"]*" *\+|`[^`]*` *\+)' --type go
```

### Network calls inside a command

Every command queued behind a command waits while it runs, so a command that waits for a mail server waits with the whole queue. Mail, HTTP calls and anything slow go in `tx.AfterCommit` or a job ([Commands](/docs/guides/commands#work-after-the-commit)). Look at matches inside commands:

```sh
rg -n 'http\.(Get|Post)|smtp\.|\.Send\(' --type go
```

## Reads

- **A render that opens its own transactions.** A render reads through the transaction it is given: `sqlite.QuerierFrom(ctx)`, or a store whose `db.View(ctx, fn)` joins the render's. One that opens transactions of its own, one for the list and one per open dialog, mixes snapshots and shows two moments at once.
- **`db.W` outside the writer and the migrations.** `db.R` is `query_only` and refuses writes, so a write through it fails loudly; one through `db.W` competes with the writer.
## Events

### A render that replays a stream

A view that reads a stream's events with `events.Load`, or the log with `events.Since` from its start, grows with the history and reads all of it at every change. Render from a projection; a feed reads `Since` from a position near the end ([Event sourcing](/docs/guides/events#reading-events)).

```sh
rg -n 'events\.(Load|Since)\(' --type go -g '!*_test.go'
```

### A write to the event tables

`lith_events`, `lith_event_state` and `lith_projections` belong to `lith/events`. A statement of the app's own that writes them skips the version check, the state row and the projectors, and an `UPDATE` changes history that projections were built from.

```sh
rg -n '(INSERT INTO|UPDATE|DELETE FROM) lith_(events|event_state|projections)' --type go
```

### A projector that writes a table it does not name

A rebuild runs a projector's handlers over every event into copies of the tables it names in `Tables`. A handler that also writes another table writes it again for every event, so a projector with tables writes only those ([Event sourcing](/docs/guides/events#rebuilds)).

## Requests

### A GET that writes

A GET handler writes nothing: no session row, no event, no counter per hit. Crawlers multiply every write a GET makes. Sessions start as a cookie, and a row appears with the first command that needs one.

### Client addresses read from headers

Client addresses come from `edge.ClientIP`, which trusts the proxy headers only from the local proxy, and takes the last `X-Forwarded-For` address before `X-Real-IP`. A handler that reads the headers itself lets any client choose its address, to dodge a rate limit or spend someone else's.

```sh
rg -n 'r\.Header\.Get\("X-(Real-IP|Forwarded-For)"\)' --type go
```

The proxy must set `X-Forwarded-For`: Caddy's `reverse_proxy` does by default, nginx with `$proxy_add_x_forwarded_for`. Caddy passes on an `X-Real-IP` the client sent unless the site block sets it, so a check that believes `X-Real-IP` first can be dodged. The template's Caddy block replaces both ([The request edge](/docs/guides/edge#client-addresses)).

### Routes without the CSRF check, or without authentication

Every route that changes data passes `edge.CSRF`, and the routes it exempts are listed with a reason. Every such route also checks that the session may do what it asks: the CSRF check says where a request comes from, and only the session says who sent it.

### A fixed window where a promise needs a sliding one

A limit promised to people, such as five messages an hour, is `edge.NewSliding(5, time.Hour)`. A fixed window lets up to twice as many through around the edge between two windows, and a bucket (`edge.NewLimiter`) gives tokens back as time passes.

```sh
rg -n 'edge\.NewWindow\(' --type go
```

## Streams

### A stream outside lith/live

A stream written outside `lith/live` lacks the tick, the dropping of stale frames and the window settings. Datastar's Go SDK given `WithBrotli` without `WithBrotliLGWin` keeps a 4 MiB window: 9.9 MiB of heap per open tab, measured.

```sh
rg -n 'datastar\.NewSSE|WithBrotli\(' --type go
```

### A stream without a cap or a deadline

Every stream counts against a cap, every write to the network has a deadline, and shutdown cancels the streams. A slot or a lock held across a network write lets a few stalled clients stop every stream.

### A stream that does not come back after a deploy

A page's stream opens with `retry: 'error'` from an element whose id is the stream's `Spec.Reconnect`. `'auto'`, Datastar's default, leaves pages stale after a deploy; `'always'` also reopens streams the server ended on purpose ([Datastar notes](/docs/datastar)).

```sh
rg -n "retry: '(always|auto)'|@(get|post)\('[^']*(stream|sse|live)" -g '*.templ' -g '*.go'
```

### A server base context that ends with the signal

The server's `BaseContext` must not end with the signal. Request contexts would end before the hub stops, the streams would end without `sse.Reconnect`, and Chrome takes a stream cut that way for a clean end, so even `retry: 'error'` does not reopen it. Stop the hub first, and give the server `context.WithoutCancel`, or no base context at all.

```sh
rg -n 'BaseContext' --type go
```

### Tab state in a map

A tab's state lives in SQLite, written by commands. A map of tabs in memory is empty after every deploy, and the pages that reconnect find nothing.

```sh
rg -n 'map\[string\]\*?[tT]ab' --type go
```

### Commands and views that disagree on topics

Every view names its topics, and every command touches the topics it changes. A command that touches nothing leaves pages stale; a view that listens to a topic every commit touches renders on every commit. `hub.Stats().RendersByKey` shows the second.

## Schema

### Random keys on large tables

Random keys scatter inserts over the B-tree: [Anders Murphy measured](https://andersmurphy.com/2026/06/05/the-perils-of-uuid-primary-keys-in-sqlite.html) inserts with UUIDv4 keys 14 to 16 times slower than with integer keys, at 10 million rows. Prefer `INTEGER PRIMARY KEY`; time-ordered ids will do. Random tokens go on small tables, and a bulk insert sorts its batch by key first.

```sh
rg -n 'uuid\.(New|NewString)|ulid\.Make' --type go
rg -n 'TEXT PRIMARY KEY' -g '*.sql'
```

### A table that grows without a sweep

Every table that grows has a rule for what it keeps, and a sweep that enforces it. Look for the call that starts each sweep: a sweeper that `main` never starts deletes nothing ([Migrations, jobs and imports](/docs/guides/migrations-and-jobs#sweeps)).

## Operations

- **Backups.** Litestream with a restore that is verified, or `VACUUM INTO` copies on a timer, copied off the machine. A `cp` of a WAL database is not a backup ([Backups](/docs/guides/backups)).
- **One owner for checkpoints.** Litestream when it is attached, the writer otherwise.
- **Migrations.** Numbered files, applied by `user_version`, one transaction each. Two branches never use one number for different files.

## The skill and this page

lith's skill, in `skills/lith`, holds the same checklist for coding agents, as `references/review.md`, terse and with the projects' names. This page is written for people and is not generated from it. Instead, the docs' `TestSkill` fails when a command of the skill's checklist is missing here: a check added to the skill has to arrive on this page in the same change. A check added here belongs in the skill as well.
