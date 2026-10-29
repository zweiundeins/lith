---
title: Wiring
headline: Wiring a lith app: database, writer, hub and edge
summary: Open the database, start the writer and the hub, put the edge around the routes, and stop them in the right order.
description: Open the SQLite database, start the writer and the live hub, put the edge around the routes, and stop them in the right order when the process ends.
order: 1
---

All of this happens once, in `main`. The template's `cmd/app/main.go` holds it, with a flag for each setting.

## The database

```go
db, err := sqlite.Open(ctx, "data/app.db", sqlite.Options{
	Migrations: migrations.FS,             // NNN_name.sql files, applied in order
	Checkpoint: sqlite.CheckpointWriter,   // the writer checkpoints between batches
	Readers:    runtime.GOMAXPROCS(0) + 4, // the hub's workers hold one each
})
```

`sqlite.Open` opens one write connection and a pool of read connections on the same file, in WAL mode. The readers open the file read-only (`mode=ro`) and are `query_only` besides, so a statement that turns `query_only` off first still cannot write it; one can still `ATTACH` another file, which neither covers, so SQL texts come from constants and values are bound. It applies the migrations numbered above the database's `user_version`, each in a transaction of its own, and refuses a database that is newer than its newest file. `db.W` belongs to the writer and the migrations; everything else reads through `db.R`.

`synchronous` is NORMAL unless you set it: SQLite syncs its log only at checkpoints, so a power loss or a kernel crash can undo up to about 35 seconds of commits, while a crash of the process loses none. With `Synchronous: "FULL"` every batch waits for its own fsync. `Synchronous: "OFF"` syncs nothing, so a power loss can corrupt the file: it is for databases that die with the process, such as a test's, where it saves the time a slow disk spends in fsync.

## The writer and the hub

```go
hub := live.New(live.Options{
	Read:                db.R,
	MaxStreams:          200,
	MaxStreamsPerClient: 16,
	SSE:                 sse.Options{Budget: sse.NewBudget(sse.BudgetOptions{Limit: 224 << 20})},
	Log:                 log,
})
w, err := writer.New(db, writer.Options{OnCommit: hub.Changed, Log: log})
```

The writer takes the write connection over. After each batch commits, it hands `OnCommit` the topics that the batch's commands touched, and `hub.Changed` marks every view that listens to one of them dirty. The hub renders dirty views in ticks, on workers that each hold a read connection for the hub's life, and sends the frames to the streams that watch them. `MaxStreams` caps the open streams: the next one gets a 503. `MaxStreamsPerClient` caps those of one client, by the key the stream handler puts in `live.Spec.Client` (`edge.ClientKey(r)`, the client's address): the next one gets a 429, so one address cannot take every place with idle streams.

The budget is the memory the streams' encoders and the frames the hub keeps may hold together; it shares it per stream, which the cap per client keeps one client from hoarding. Without a `Limit` it takes half of GOMEMLIMIT or of the cgroup's memory limit, and in a process with neither a quarter of the machine's memory. `live.New` logs the limit and where it came from. Set it, or GOMEMLIMIT, to what the host gives the app; the template's unit sets GOMEMLIMIT.

The read pool needs a connection for every worker (GOMAXPROCS of them by default) and some for GET requests besides. `Readers` defaults to GOMAXPROCS + 4, and at least 8.

## Without lith's writer

An app that keeps its own store and commits through its own transactions can use `live` and `sse` without `writer`. The hub knows nothing of the writer: `OnCommit` only calls `hub.Changed`, so the app calls it itself after each commit, with the topics the commit changed:

```go
if err := tx.Commit(); err != nil {
	return err
}
hub.Changed([]string{"card:7", "board:1"})
```

Call it once the commit is visible to readers, never inside the transaction. `Changed` takes the next commit number (`hub.Seq()`) and marks the views dirty. A tick that begins after it reads that number before its read transaction begins, and counts every commit up to it as part of its snapshot; so does a cache, through `live.Seq(ctx)` or `hub.Seq()`. A `Changed` before the commit is visible lets a render read the old rows under the new number: the view sends a stale frame and is clean again, and a cache stores the old value as good as of the new number and keeps it while the topics stay quiet. Visible means that a read transaction the hub's read pool begins now sees the rows.

Several commits can share one call, with the union of their topics, made after the last of them is visible, as the writer does for the commands of one batch. Until that call, the pages show the state before the first of them. A change that no topic tracks takes `hub.MarkAll()`.

## The edge

```go
h := http.Handler(site.Routes())
h = edge.Sessions{Secure: secure}.Handler(h)
h = edge.CSRF{Datastar: []string{"POST /cmd/", "POST /pace", "POST /ack"}}.Handler(h)
h = edge.SecurityHeaders{CSP: edge.DefaultCSP(), HSTS: secure}.Handler(h)
h = edge.Compress(h)
h = edge.Recover(log, h)
h = edge.LogRequests(log, h)
```

The chain is built from the inside out. `Sessions` hands every browser an opaque id in a cookie and stores nothing: a row appears with the first command that needs one, so a crawler's GET costs nothing. `CSRF` refuses cross-site actions, and wants Datastar's `Datastar-Request` header on the action routes. `SecurityHeaders` sets a Content Security Policy with a new nonce for each request, `'unsafe-eval'` included, since Datastar compiles its expressions. `Compress` compresses GET answers; streams compress themselves.

The health checks stay outside the edge, so probes get no cookie and no log line. `edge.Health(version)` at `/healthz` answers as long as the process serves, and `edge.Ready` at `/readyz` runs the checks the app registers, such as a `SELECT 1` on the read pool, and `edge.Static.Warmed`, which passes once the static files are compressed.

The static files stay outside `Sessions` too, behind `SecurityHeaders`, `Recover` and `LogRequests`: their answers are public and immutable, and a session cookie on one would go to everyone a shared cache hands it to. `Sessions` mints a cookie for every request without one.

The server's `ReadTimeout`, 15 s, gives a request's headers and body that long to arrive, also on a route that never reads the body, where a client that trickles one would otherwise hold its connection; it leaves streams alone, and an upload extends it with `edge.Upload` ([Request bodies](/docs/guides/edge#request-bodies)).

## Starting and stopping

```go
go w.Run(context.Background()) // not the signal context
go hub.Run(hubCtx)
```

The writer runs on a context that outlives the HTTP server. Once `w.Run`'s context ends, `Send` and `Do` return `ErrClosed`, and every action that arrives while the server drains would fail. On SIGTERM, the template stops in this order:

1. Cancelling `hubCtx` ends every stream. Each one's last event is `sse.Reconnect`, so the page opens its stream again on the next process.
2. `srv.Shutdown` lets the running requests finish, and their actions still reach the writer.
3. `w.Close()` applies what is queued, runs `PRAGMA optimize` and gives the connection back.
4. `db.Close()` closes both pools.

The template also starts an hourly sweep from `main`, on the signal's context: it deletes the state of tabs gone for a day, entries beyond the newest thousand and old op ids. Every table that grows needs a sweep, and a sweep only works if something starts it.
