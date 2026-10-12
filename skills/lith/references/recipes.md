# Recipes

Each recipe is the smallest code that does the job the lith way. Names and
signatures are lith's; check `go doc github.com/zweiundeins/lith/<package>` when
in doubt.

## Wire a project

```go
db, err := sqlite.Open(ctx, "data/app.db", sqlite.Options{
	Synchronous: "FULL",             // or "NORMAL": a durability choice, see performance.md
	Migrations:  migrations,         // fs.Sub(embedded, "migrations"): NNN_name.sql files
	Readers:     runtime.GOMAXPROCS(0) + 4, // the render workers hold one each, GETs need the rest
	Checkpoint:  sqlite.CheckpointWriter,    // CheckpointExternal when Litestream is attached
})
hub := live.New(live.Options{Read: db.R})
w, err := writer.New(db, writer.Options{OnCommit: hub.Changed})

go w.Run(appCtx)   // appCtx outlives the HTTP server: actions still write while it drains
go hub.Run(hubCtx) // cancelling hubCtx ends every stream at once
```

Shutdown, in this order: cancel `hubCtx` (streams end, so `Shutdown` does not
wait for them), `srv.Shutdown` (actions finish and still reach the writer),
`w.Close()` (the queue drains), `db.Close()` (optimize, checkpoint, close).

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
  at once (`ErrQueueFull` when the queue is full) and logs failures.
- Build SQL from constants: the writer prepares each SQL text once.
- Scan every `QueryRow`: an unscanned row keeps its cached statement busy.
- A retried request (a flaky network, a double click) runs once with
  `writer.Once(opID, cmd)`. Only successes are recorded; a refusal runs again
  and refuses again.
- Mail, HTTP calls and anything slow go in `tx.AfterCommit` or outside the
  writer. A command never waits for the network.

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
- The page opens its stream with `data-init` and `retry: 'always'`.

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

## Measuring a change

- In process: `lith/loadtest` runs the writer and the hub with N streams and
  M producers and reports tick percentiles, renders per tick, bytes per frame
  and heap per stream. Run it before and after.
- End to end: kanban's `cmd/netem` and `cmd/loadtest`.
- Write the numbers into `benchmarks/RESULTS.md` with date, machine and the
  exact command.
