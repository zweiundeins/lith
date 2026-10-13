# Recipes

Each recipe is the smallest code that does the job the lith way. Names and
signatures are lith's; check `go doc github.com/zweiundeins/lith/<package>` when
in doubt.

## Start a new project

```sh
go run github.com/zweiundeins/lith/cmd/lith-new -lith /develop/lith ~/develop/guestbook example.com/acme/guestbook
cd ~/develop/guestbook && go mod tidy && go tool task test
```

The template is a guestbook that uses every rule once (a command, a shared
view, a per-tab view backed by `tab_state`, a pending button), with tests,
deploy files (systemd, Caddy, a deploy script with rollback, a backup timer)
and CI. Replace the guestbook with the app, keep the wiring. Until lith is
published, go.mod needs the `replace` that `-lith` writes, and CI fails while
it points at a local path.

## Wire a project

```go
db, err := sqlite.Open(ctx, "data/app.db", sqlite.Options{
	Synchronous: "FULL",                  // or "NORMAL": a durability choice, see performance.md
	Migrations:  migrations,              // fs.Sub(embedded, "migrations"): NNN_name.sql files
	Checkpoint:  sqlite.CheckpointWriter, // CheckpointExternal when Litestream is attached
})
hub := live.New(live.Options{Read: db.R})
w, err := writer.New(db, writer.Options{OnCommit: hub.Changed})

go w.Run(context.Background()) // not the signal context: see below
go hub.Run(hubCtx)             // cancelling hubCtx ends every stream at once
```

Shutdown, in this order: cancel `hubCtx` (streams end, so `Shutdown` does not
wait for them), `srv.Shutdown` (actions finish and still reach the writer),
`w.Close()` (the queue drains), `db.Close()` (optimize, checkpoint, close).
Ending `w.Run`'s context drains the queue like `w.Close()` and makes Send and
Do return `ErrClosed` from then on, so a writer on the signal context refuses
the actions that arrive while the server drains.

The read pool's default size, GOMAXPROCS + 4 and at least 8, is the hub's
render workers (GOMAXPROCS, each holding a connection for good) plus four or
more for GET documents and other reads. Set `Readers` higher when more reads
run at once besides the hub.

A project with its own migration runner (a `schema_migrations` table, a list
of versions in Go) passes it in `Migrate` instead of `Migrations`: Open calls
it with the write connection before the read pool opens.
`sqlite.Migrate(ctx, db, fsys)` is the built-in runner, for a test or a tool.

A CLI that only reads opens the file with `sqlite.Options{ReadOnly: true}`:
no write connection, no migrations, and a missing file is an error instead
of a new empty database.

To time every statement and log the slow ones, register dbmetrics' wrapper
of modernc's driver once and name it in `Driver`: the write connection, the
read pool and `Backup` then all open through it.

```go
m := prom.New("app") // github.com/zweiundeins/lith/dbmetrics/prom
prometheus.MustRegister(m)
if err := dbmetrics.Register("sqlite-timed", "sqlite", dbmetrics.Options{Observer: m}); err != nil {
	return err
}
db, err := sqlite.Open(ctx, "data/app.db", sqlite.Options{Synchronous: "FULL", Driver: "sqlite-timed"})
```

The request edge, built from the inside out:

```go
h := http.Handler(mux)
h = edge.Sessions{Secure: !dev}.Handler(h)
h = edge.CSRF{Datastar: []string{"POST /cmd/"}}.Handler(h)
h = edge.SecurityHeaders{CSP: edge.DefaultCSP()}.Handler(h)
h = edge.Compress(h)
h = edge.Recover(log, h)
h = edge.LogRequests(log, h)
```

## A command

A command is a value with `Apply`. Checks that read before they write go
inside it, so they run in the same transaction as the write.

```go
type MoveCard struct{ Card, Lane, Seen int64 }

func (c MoveCard) Validate() error {
	if c.Card <= 0 || c.Lane <= 0 {
		return errors.New("card and lane are required")
	}
	return nil
}

func (c MoveCard) Apply(tx *writer.Tx) error {
	var board, version int64
	err := tx.QueryRow(`SELECT board_id, version FROM cards WHERE id = ?`, c.Card).Scan(&board, &version)
	if err != nil {
		return err
	}
	if version != c.Seen {
		return ErrMovedMeanwhile // the page shows why; nothing changed
	}
	if _, err := tx.Exec(`UPDATE cards SET lane_id = ?, version = version + 1 WHERE id = ?`, c.Lane, c.Card); err != nil {
		return err
	}
	tx.Touch(fmt.Sprintf("board:%d", board))
	return nil
}
```

The handler validates, sends and answers 204. The outcome reaches the page on
its stream.

```go
func (s *Server) moveCard(w http.ResponseWriter, r *http.Request) {
	cmd, err := readMove(r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.w.Do(r.Context(), cmd); err != nil && !errors.Is(err, ErrMovedMeanwhile) {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

- `Do` waits for the commit and returns this command's error; `Send` returns
  at once (`ErrQueueFull` when the queue is full). A failed Send goes to
  `writer.Options.OnSendError(cmd, err)` on the writer's goroutine, or to the
  log without one.
- `w.DoValue(ctx, fn)` is `Do` for a function that returns a value, such as
  a new row's id: `id, err := w.DoValue(ctx, func(tx *writer.Tx) (int64,
  error) {…})`. It returns the value once the batch has committed, and the
  zero value with any error.
- Commands run in the order they arrive. `w.Flush(ctx)` returns once every
  command queued before it is applied: a test sends, flushes, then reads.
- Build SQL from constants: the writer prepares each SQL text once, and
  keeps the 512 used last. SQL built at runtime, such as an IN list with a
  mark per value, goes through `tx.ExecUncached`, `tx.QueryUncached` or
  `tx.QueryRowUncached`, which keep no statement.
- Scan every `QueryRow` before the same query runs again: an unscanned row
  keeps its cached statement busy.
- A retried request (a flaky network, a double click) runs once with
  `writer.Once(opID, cmd)`. Only successes are recorded; a refusal runs again
  and refuses again.
- Mail, HTTP calls and anything slow go in `tx.AfterCommit` or outside the
  writer. A command never waits for the network.

A retry that must get the first answer, a refusal included, goes through
`writer.OnceResult`. A refusal is a `*writer.Refusal`: its changes are undone
like any error's, but the key is recorded with its message and the result.
Other errors record nothing, so a retry runs the command again.

```go
op := writer.OnceResult(opID, writer.ResultFunc(func(tx *writer.Tx) ([]byte, error) {
	var free int
	if err := tx.QueryRow(`SELECT free FROM lanes WHERE id = ?`, lane).Scan(&free); err != nil {
		return nil, err
	}
	if free == 0 {
		return nil, &writer.Refusal{Msg: "This list is full."}
	}
	res, err := tx.Exec(`INSERT INTO cards (lane_id, title) VALUES (?, ?)`, lane, title)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	return strconv.AppendInt(nil, id, 10), err
}))
err := s.w.Do(r.Context(), op) // the same *writer.Refusal on every retry
```

After `Do`, `op.Result()` holds the first run's result (the card's id) and
`op.Replayed()` tells a retry. `writer.PruneOnce` forgets old keys of both.

## Query code for reads and commands

A query written once against `sqlite.Querier` serves a GET, a view and a
command.

```go
func laneCount(ctx context.Context, q sqlite.Querier, lane int64) (n int, err error) {
	err = q.QueryRowContext(ctx, `SELECT count(*) FROM cards WHERE lane_id = ?`, lane).Scan(&n)
	return n, err
}
```

```go
n, err := laneCount(tx.Context(), tx, lane) // in a command

q, _ := sqlite.QuerierFrom(ctx) // in a view's Render
n, err := laneCount(ctx, q, lane)

n, err := db.ViewValue(ctx, func(tx *sql.Tx) (int, error) { // in a GET
	return laneCount(ctx, tx, lane)
})
```

`db.ViewValue` is `db.View` for a function that returns a value; it returns
the zero value with an error. Several results go in a struct, or stay with
`db.View` and variables outside the function.

A `*writer.Tx` runs the query on the batch's transaction with the writer's
statement cache; the ctx it gets cancels nothing. In a view, the worker runs
each SQL text on a statement it prepared once on its connection, in the
render's snapshot.

A store that takes only a ctx reads through `db.View(ctx, fn)`. Called from a
render, View runs fn in the render's transaction, when it reads the same
database file, instead of opening a second snapshot; `sqlite.WithTx(ctx, tx)`
hands a GET's transaction down the same way.

## A view

A view is one region of a page, rendered once per tick for everyone who sees
it.

```go
func boardView(id int64) live.View {
	key := fmt.Sprintf("board:%d", id)
	return live.View{
		Key:    key,
		Topics: []string{key},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			b, err := queries.Board(ctx, tx, id) // every query on this tx: one snapshot
			if err != nil {
				return nil, err
			}
			var buf bytes.Buffer
			if err := views.Board(b).Render(ctx, &buf); err != nil {
				return nil, err
			}
			return sse.PatchElements(buf.Bytes(), sse.Patch{Selector: "#board"}), nil
		},
	}
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	tab := readTabID(r)
	s.hub.Serve(w, r, live.Spec{ID: tab, Views: []live.View{boardView(id), s.tabView(sid, tab)}})
}
```

- Nothing about the viewer goes into a shared view. Per-user parts (the
  tab's outcomes, "you" in a presence list, a toast) go into a view keyed by
  the tab, or come from CSS on a data attribute.
- `Every: time.Second` re-renders a view without commits (clocks, live stats).
  `Presence: true` re-renders it when streams join or leave; `hub.Count(key)`
  is how many watch.
- `Spec.Write` tailors a shared frame per stream (kanban sends only the lanes
  a stream has not seen). Keep it free of formatting: format once in Render.
  `Spec.WriteTick(s, frames)` gets all of a stream's new frames at once, in
  the order it watches the views: one `s.Write(events...)` of them is one
  compressed write and flush per tick instead of one per view.
- A page's command to watch a view (a panel that scrolls into view) can
  arrive before its stream. `hub.Watch(tab, v)` then keeps v for
  `Options.PendingWatch` (10 s), and the stream that opens with that ID
  watches it from the start; Unwatch drops it again. Watch reports whether
  the stream is open now, so a retry loop is not needed.
- The page opens its stream with `data-init` and `retry: 'always'`. A
  watchdog on the page needs a heartbeat it can see: with
  `Options.HeartbeatSignal: "_ping"` the hub patches `$_ping` every
  Heartbeat (20 s), frames or not, with a number that grows, instead of an
  SSE comment, which Datastar never shows the page.

## Tab state and preferences

State that must survive a deploy is a row, written by a command.

```sql
CREATE TABLE tab_state (
    sid    TEXT NOT NULL,
    tab_id TEXT NOT NULL,
    data   TEXT NOT NULL,     -- JSON of the tab's view state
    seen   INTEGER NOT NULL,  -- unix seconds, for the sweep
    PRIMARY KEY (sid, tab_id)
) WITHOUT ROWID;
```

A command upserts it and touches `tab:<id>`; the tab's view renders from it.
Preferences that outlive a page (a sort order, a theme preview) go in a table
keyed by the session and touch `sid:<id>`. A background command deletes tab
rows not seen for a few days.

## A background job

```go
t := time.NewTicker(time.Hour)
for range t.C {
	rows, err := fetchUpstream(ctx) // network outside the writer
	if err != nil {
		log.Warn("fetch", "err", err)
		continue
	}
	if err := w.Do(ctx, ImportRows{Rows: rows}); err != nil {
		log.Warn("import", "err", err)
	}
}
```

Start every job from `main` on the app context, and sweep every table that
grows. A job that nobody starts is the same as no job.

## A bulk import

Split it into commands of a few thousand rows, so other commands get their
turn between them, and sort each chunk by primary key: random keys scatter
inserts over the B-tree, sorted ones append.

```go
slices.SortFunc(rows, func(a, b Row) int { return bytes.Compare(a.Key, b.Key) })
for chunk := range slices.Chunk(rows, 5000) {
	if err := w.Do(ctx, InsertRows{Rows: chunk}); err != nil {
		return err
	}
}
```

## Work a transaction cannot hold

`PRAGMA foreign_keys` does nothing inside a transaction, and ATTACH, DETACH
and VACUUM are refused there. `w.Exclusive(ctx, fn)` runs fn on the writer's
own connection between two batches, outside any transaction; commands queued
meanwhile wait and run after it in order. It replaces a second write
connection, such as libretto's Legacy (factory reset, tenant import and
merge): a transaction on that one that runs past the busy timeout makes the
writer's batches fail with `SQLITE_BUSY`.

```go
err := w.Exclusive(ctx, func(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS src`, path); err != nil {
		return err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `DETACH DATABASE src`)
	tx, err := conn.BeginTx(ctx, nil) // IMMEDIATE, like every transaction on the write connection
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO cars SELECT * FROM src.cars`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO trips SELECT * FROM src.trips`); err != nil {
		return err
	}
	return tx.Commit()
})
if err == nil {
	hub.Changed([]string{"cars"}) // Exclusive reaches no OnCommit
}
```

- fn restores every pragma it changes, on its error paths too: a `defer`
  with `context.WithoutCancel(ctx)` still runs when the request is gone. It
  ends its transactions and closes its rows; a transaction it leaves open is
  rolled back and reported.
- fn never waits for the writer (`Do`, `Flush`, another `Exclusive`): the
  writer is waiting for fn.
- Every command waits while fn runs. An import that a command's transaction
  can hold goes through commands in chunks instead, as above.

## An event-sourced command

The writer serialises commands, so there is no optimistic retry loop. One
`Apply` loads the aggregate (from the projection, plus newer events if the
projection can lag), decides, appends the events and updates the projection.
Renders read the projection, never the event stream.

```go
func (c Record) Apply(tx *writer.Tx) error {
	s, err := loadSession(tx, c.Session) // session_view row
	if err != nil {
		return err
	}
	events, err := decide(s, c) // pure
	if err != nil {
		return err
	}
	for _, e := range events {
		if _, err := tx.Exec(`INSERT INTO events(stream_id, type, data) VALUES (?, ?, ?)`, c.Session, e.Type, e.Data); err != nil {
			return err
		}
		s = s.Apply(e)
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO session_view(id, data) VALUES (?, ?)`, c.Session, s.JSON()); err != nil {
		return err
	}
	tx.Touch("session:" + c.Session)
	return nil
}
```

## Backups

- Without Litestream: `db.Backup(ctx, path)` (VACUUM INTO on its own
  read-only connection) from a timer, rotated, copied off the machine.
- With Litestream: the nested module `github.com/zweiundeins/lith/backup`
  restores a missing file before `sqlite.Open`, attaches, verifies a restore
  daily; open the database with `sqlite.CheckpointExternal` so Litestream
  owns the checkpoints.
- A check of a copy opens it with `sqlite.Options{ReadOnly: true}` and runs
  `PRAGMA integrity_check` on `db.R`: nothing is written, and a missing copy
  is an error.

## Measuring a change

- In process: `lith/loadtest` runs the writer and the hub with N streams and
  M producers and reports tick percentiles, renders per tick, bytes per frame
  and heap per stream. Run it before and after.
- `hub.Stats()` counts the workers' read transactions (`ReadTxs`, at most one
  per worker and tick) and the renders of each view watched now
  (`RendersByKey`): a view that renders more often than its topics change
  listens to too much.
- End to end: kanban's `cmd/netem` and `cmd/loadtest`.
- Write the numbers into `benchmarks/RESULTS.md` with date, machine and the
  exact command.
