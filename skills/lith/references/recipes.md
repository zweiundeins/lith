# Recipes

Each recipe is the smallest code that does the job the lith way. Names and
signatures are lith's; check `go doc github.com/zweiundeins/lith/<package>` when
in doubt.

## Start a new project

```sh
go run github.com/zweiundeins/lith/cmd/lith-new@latest ~/develop/guestbook example.com/acme/guestbook
cd ~/develop/guestbook && go mod tidy && go tool task test
```

The template is a guestbook that uses every rule once (a command, a shared
view, a per-tab view backed by `tab_state`, a pending button), with tests,
deploy files (systemd, Caddy, a deploy script with rollback, a backup timer,
the probes and log shipping a shared host loads) and CI. Replace the guestbook with the app, keep the wiring. For work across
checkouts, `lith-new -lith <dir>` writes a `replace` directive to a local
lith instead; CI fails while it points at a local path.

## Wire a project

```go
db, err := sqlite.Open(ctx, "data/app.db", sqlite.Options{
	Migrations: migrations,              // fs.Sub(embedded, "migrations"): NNN_name.sql files
	Checkpoint: sqlite.CheckpointWriter, // CheckpointExternal when Litestream is attached
	// Synchronous defaults to NORMAL: a power loss can undo up to about 35 s
	// of commits. "FULL" where it must not (see performance.md).
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

An app that keeps its own store and does not use lith's writer still uses
`live` and `sse`: it calls `hub.Changed(topics)` itself after each commit,
once readers see it, never inside the transaction. A render or a cache that
reads `Seq` after the call counts the commit as part of its snapshot; a call
before the commit is visible lets it store the old rows under the new number.
Several commits can share one call, with the union of their topics, made
after the last of them.

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
// lith's own numbers (github.com/zweiundeins/lith/prom): the writer's
// batches, the hub's ticks and frames, the stream budget.
prometheus.MustRegister(lithprom.New("app", lithprom.Sources{Writer: w, Hub: hub}))
if err := dbmetrics.Register("sqlite-timed", "sqlite", dbmetrics.Options{Observer: m}); err != nil {
	return err
}
db, err := sqlite.Open(ctx, "data/app.db", sqlite.Options{Driver: "sqlite-timed"})
```

The request edge, built from the inside out:

```go
h := http.Handler(mux)
h = edge.Sessions{Secure: !dev}.Handler(h)
h = edge.CSRF{Datastar: []string{"POST /cmd/", "POST /pace", "POST /ack"}}.Handler(h)
h = edge.SecurityHeaders{CSP: edge.DefaultCSP()}.Handler(h)
h = edge.Compress(h)
h = edge.Recover(log, h)
h = edge.LogRequests(log, h)
```

Static files go out under hashed, immutable URLs, precompressed, and
minified with `edge.Minify(minify.Bytes)` (`github.com/zweiundeins/lith/minify`,
a nested module). Leave the option out in development (the template's
`-dev`), so the browser's devtools show the files as written:

```go
var opts []edge.StaticOption
if !dev {
	opts = append(opts, edge.Minify(minify.Bytes))
}
static, err := edge.NewStatic(files, "/static/", opts...) // mount at GET /static/
url := static.URL("style.css")                           // /static/style.3f2a9c1b2d4e.css
```

A file named `*.min.js` or `*.min.css`, or one that minifying shrinks by
less than a twentieth, goes out as written: a vendored build keeps the
bytes its integrity hash pins.

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

The handler validates, hands the command over with `Do` and answers 204. The
outcome reaches the page on its stream.

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
- `Do` for what a person asked for: its 204 means stored, and a failed batch
  or a stopping server answers an error, so a pending button never waits
  forever for its op id. It costs about half a millisecond with NORMAL, and
  waiting handlers hold the load back. `Send` only for updates the next one
  replaces and nobody waits for (a pointer's position, a heartbeat): the
  site's cursor party.
- Every outcome a command reaches, a refusal included, goes into the tab's
  state and over the stream. The status code says only that the request
  reached no outcome (never arrived, turned away by a limit or check, the
  writer failed, the server stops): the page shows "did not go through" from
  `datastar-fetch`'s error. Never write those failures to the stream: it may
  be down too, and storing them needs the writer that failed.
- `w.DoValue(ctx, fn)` is `Do` for a function that returns a value, such as
  a new row's id: `id, err := w.DoValue(ctx, func(tx *writer.Tx) (int64,
  error) {…})`. It returns the value once the batch has committed, and the
  zero value with any error.
- Commands run in the order they arrive. `w.Flush(ctx)` returns once every
  command queued before it is applied: a test sends, flushes, then reads.
- Build SQL from constants: the writer prepares each SQL text once, and
  keeps the 512 used last. SQL built at runtime, such as an IN list with a
  mark per value, goes through `tx.ExecUncached`, `tx.QueryUncached` or
  `tx.QueryRowUncached`, which keep no statement. Query code shared with
  reads passes `sqlite.Uncached(ctx)` instead, which the writer honours too.
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

Reads keep statements too: each read connection the last 128 SQL texts it
ran (`sqlite.Options.Statements`), each render worker the last 512. SQL built
at runtime, such as an IN list sized to the results or a sort the request
picks, runs with a ctx from `sqlite.Uncached(ctx)`: it is prepared for that
run and closed with its rows, and takes no place in any cache, on `db.R`, in a
`db.View` or a render's tx, through `sqlite.QuerierFrom(ctx)`, or on a
`*writer.Tx`.

```go
marks := strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ")
rows, err := q.QueryContext(sqlite.Uncached(ctx), `SELECT id, title FROM cards WHERE id IN (`+marks+`)`, ids...)
```

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
	s.hub.Serve(w, r, live.Spec{ID: tab, Views: []live.View{boardView(id), s.tabView(sid, tab)}, Reconnect: "stream"})
}
```

```html
<div id="stream" hidden data-init="@get('/stream?tab=' + $_tab, {retry: 'error', retryMaxCount: Infinity})"></div>
```

- Nothing about the viewer goes into a shared view. Per-user parts (the
  tab's outcomes, "you" in a presence list, a toast) go into a view keyed by
  the tab, or come from CSS on a data attribute.
- `Every: time.Second` re-renders a view without commits (clocks, live stats).
  `Presence: true` re-renders it when streams join or leave; `hub.Count(key)`
  is how many watch, `hub.Watchers(key)` the IDs of their streams, in the
  order they opened. A stream's ID is its tab's: render a hash of it, never
  the ID, since whoever knows it can open a stream that ends the tab's own.
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
- The page opens its stream with `data-init` on the element that
  `Spec.Reconnect` names, with `retry: 'error'`: a deploy ends the stream
  with `sse.Reconnect`, which runs that data-init again, and `'error'`
  retries until the next process answers (datastar.md). A stream that ends
  without it, such as one a newer stream from the same tab replaced, stays
  closed. A handler that streams without the hub ends with
  `st.CloseWith(sse.Reconnect(id))` when the server stops. A
  watchdog on the page needs a heartbeat it can see: with
  `Options.HeartbeatSignal: "_ping"` the hub patches `$_ping` every
  Heartbeat (20 s), frames or not, with a number that grows, instead of an
  SSE comment, which Datastar never shows the page.
- A page on a slow link asks for a slow stream with `Save-Data: on`, at
  the same URL: the template measures once into `$_slow` (data-signals:
  navigator.connection's saveData or 2g/3g, else Navigation Timing), its
  `@get` sends `headers: $_slow ? {'Save-Data': 'on'} : {}`, its
  documents send `Accept-CH: edge.NetworkHints`, and its handler passes
  `Slow: edge.SlowLink(r)` in the Spec. lith's `newest-frame.js`
  (`live.Scripts`, `live.NewestFrame`, vendored from
  github.com/mbolli/datastar-attribute-newest-frame) keeps a page that
  falls behind close to the present: add it to the app's `edge.Static`
  (`Add("lith/"+live.NewestFrame, ...)`), load it as a module after
  Datastar through the import map that maps `'datastar'` (with the CSP
  nonce), and put `data-newest-frame` on `<body>` (performance.md,
  datastar.md). It dispatches `newest-frame-behind` when the page drops
  most of its frames, every 3 s while behind. The template's `<body>`
  posts it (`data-on:newest-frame-behind`, `@post` with
  `requestCancellation: 'disabled'`, on `<body>`, never on the stream's
  element) to `POST /pace`, which validates the tab id, limits each
  session (`edge.NewLimiter(1, 5)`), calls `hub.Behind(id)` and
  `hub.Pace(id, 1)`, and answers 204: the hybrid in place, the default.
  Every stream has `Spec.Ack` and no `Window`, so it starts unpaced;
  `Release` (5 s) and `ReleaseSlack` (100 ms) stay at their defaults. Each
  paced write ends with an `<i>` patched inner into the stream's element
  whose `data-init` posts `POST /ack?tab=&s=&n=&rtt=` after the paint, the
  round trip from Resource Timing; `/ack` validates, limits each session
  (`edge.NewLimiter(30, 60)`), calls `hub.RoundTrip` then `hub.Ack`, and
  answers 204. The stream ID names the session and the tab
  (`streamID(sid, tab)`), built the same way in the stream handler and
  both routes, so a post reaches only its session's streams. Both routes
  go in `edge.CSRF`'s `Datastar` list. prom exports
  `<ns>_live_newest_frame_behind_total`,
  `<ns>_live_newest_frame_behind_streams_total`, `<ns>_live_paces_total`
  and `<ns>_live_releases_total`. To turn pacing off, leave `Ack` out of
  the Spec: `/pace` then only counts. `data-newest-frame__reopen` also
  sets `$_slow` and opens a slow stream of four frames a second for good:
  not the default, since at CPU 6x it held the page to 4 frames a second,
  0.37 s behind, where no fix was 0.10 s (performance.md, the streams
  guide).

## Views in a shared database, per person

When people share a database and their permissions differ, two people on
the same route see different things. The view's key carries everything its
Render filters by: the person, the route and the filters.

```go
func pagesView(person, space int64, filter string) live.View {
	return live.View{
		Key:    fmt.Sprintf("pages:%d:person:%d:q=%q", space, person, filter),
		Topics: []string{SpaceTopic(space), GrantsTopic(person)},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			q, _ := sqlite.QuerierFrom(ctx)
			pages, err := queries.Readable(ctx, q, person, space, filter) // WHERE restricted = 0 OR a grant
			if err != nil {
				return nil, err
			}
			return renderPages(pages), nil
		},
	}
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	person, ok := s.person(r) // from the session, never from the URL or a signal
	if !ok {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	v := pagesView(person, spaceID(r), r.URL.Query().Get("q"))
	s.hub.Serve(w, r, live.Spec{ID: readTabID(r), Views: []live.View{v}})
}
```

- Views with equal keys are one view: while it is watched, the definition
  it was first given renders it, once a tick, for every stream on the key.
  A key of the route alone (`pages:1`) renders the first viewer's list for
  everyone who opens the route after them, restricted titles included.
- Render gets no request, stream or session, and that is right: one render
  serves every stream on the key. The person reaches it through the key:
  closed over by the handler that built the view, or parsed out of the key
  by code that builds a view from a key the page sent (`hub.Watch`), after
  checking it against the session.
- Topics: the view listens to what it shows (`space:<id>`) and to the
  person's grants (`grants:<person>`). A new page touches the space; a
  grant touches only its person. Never a topic per person for a change
  everyone may see: the command would have to know who watches.
- Cost: one render per person who watches, per change of the space. Keep
  per-person views small; content that is the same for everyone allowed to
  see it (a page's body) goes in a view keyed by the page, watched only
  after the handler checked access, and `live.Cache` reuses row fragments
  across the per-person renders.
- Sharing a frame across people cannot be done safely in general. It needs
  a key of what they share (a role), a Render that filters by that alone
  (no name, no "you", no button by grant), and a grant change that moves
  the person's streams to the new key (`Unwatch`, `Watch`). A filter in
  `Spec.Write` reads grants outside the render's snapshot. Render per
  person until a measurement says otherwise.
- `site/internal/docs/example/access` is the compiled version, with a test
  that two people on one route get two frames and one that shows the leak
  of a route-only key.

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

## A per-tab view that navigates

A tab that can show any route keeps the route in its row, and one view keyed
by the tab renders whatever the route names. The tab's topic is the view's
static topic; what the route reads, the Render declares with
`live.Depends(ctx, topics...)` as it reads. Navigation is a command that
writes the route and touches the tab: the view renders the new route,
declares its topics, and the hub drops those of the old route. No Unwatch,
no Watch, no route in the key.

```go
func tabView(sid, tab string) live.View {
	key := "tab:" + tab
	return live.View{
		Key:    key,
		Topics: []string{key}, // the tab's row, with its route
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			q, _ := sqlite.QuerierFrom(ctx)
			st, err := queries.Tab(ctx, q, sid, tab)
			if err != nil {
				return nil, err
			}
			var page templ.Component
			switch st.Route.Kind {
			case "day":
				live.Depends(ctx, "mrs", "issues", "todos", "calendar")
				d, err := queries.Day(ctx, q, st.Route.Day)
				if err != nil {
					return nil, err
				}
				page = views.Day(d)
			case "issue":
				live.Depends(ctx, "issue:"+st.Route.ID)
				is, err := queries.Issue(ctx, q, st.Route.ID)
				if err != nil {
					return nil, err
				}
				page = views.Issue(is)
			}
			var buf bytes.Buffer
			if err := page.Render(ctx, &buf); err != nil {
				return nil, err
			}
			return sse.PatchElements(buf.Bytes(), sse.Patch{Selector: "#main"}), nil
		},
	}
}
```

```go
// Navigate is the command behind a link: it writes the route, and the
// tab's view follows.
func (c Navigate) Apply(tx *writer.Tx) error {
	_, err := tx.Exec(`UPDATE tab_state SET data = json_set(data, '$.route', json(?))
		WHERE sid = ? AND tab_id = ?`, c.Route, c.Session, c.Tab)
	if err != nil {
		return err
	}
	tx.Touch("tab:" + c.Tab)
	return nil
}
```

- Declare a topic where the query reads it: a query helper can call
  `live.Depends(ctx, "issue:"+id)` itself, so the topics follow the reads. A
  topic declared twice counts once; outside a Render, Depends does nothing.
- The topics a Render declares replace those of the Render before, once it
  returns. From the render after a navigation on, a commit to the new route's
  topics renders the tab and one to the old route's does not. A commit to a
  new topic that lands after the render's snapshot, before the hub takes the
  topic on, renders it again: the hub compares the topic's version with the
  render's `live.Seq(ctx)`.
- A Render that fails keeps the topics of the one before. What every route
  reads (the tab's row, the session's preferences) stays in `Topics`.
- A view keyed by the tab is watched by one stream, so its topics are that
  tab's alone. A shared view declares the same topics for everyone, since
  everyone gets the same render.
- A render that declares what the one before declared costs a sort and a
  compare of its topics; one that changes them, an update of the hub's tree
  per topic, about 1 µs per view for four topics in and four out
  (`BenchmarkTickDepends`).

## A topic hierarchy

Name a topic from the general to the particular, one segment per level,
joined by `:`: the kind or source first, then what holds it, then the item
(`board:42`, `board:42:card:7`, `confluence:SWE:123`). A commit counts for
every level above it: `board:42:card:7` touches `board:42` and `board`.
Each view listens at the level of what it shows:

- a page's view on `confluence:SWE:123`, for that page;
- a space's list on `confluence:SWE`, for any page in it;
- search and the counts per source on `confluence`, for anything in it.

State per tab, per session or of who is present goes under a root of its
own (`tab:<demo>:<tab>`, `sid:…`, `here:<demo>`), never under the root of
the shared data, because a view on a topic renders for every commit below
it. The site's board demo kept its presence (the avatars of who has the
board open, touched by each tab's hello) at `board:here`, below `board`:
every hello rendered the shared board, whose view transition broke a card
being dragged. It is `here:board` now.

A commit to a level does not reach the levels below it. When everything
below a level changed, as after a sync that rewrote a space, touch the
level with `:*`, once, instead of each item:

```go
tx.Touch(live.Below(live.Topic("confluence", c.Space))) // confluence:SWE:*
```

`confluence:SWE:*` marks the views of the space and of every page below it,
and counts for `confluence`; `hub.Version` of each page reads at least that
commit. `*` alone touches everything, as `hub.MarkAll()`.

- An id must not contain `:`, or it makes levels of its own
  (`urn:uuid:…`). Ids from outside the app go through
  `live.Topic(parts...)`, which escapes `:`, `*` and `%` percent-style and
  leaves plain parts alone: `live.Topic("board", "42")` is `board:42`. An
  empty part becomes `%`.
- A view's topics have no empty segment and no `*`: `Serve` and `Watch`
  panic, `Depends` fails the render. A view on `confluence:SWE` hears
  everything below it already. `hub.Changed` does not check, it runs for
  every commit.
- Aggregates key on the level they cover: a space's count on
  `hub.Version("confluence:SWE")`. The levels above the named topics have
  their own store (`live.Options.PrefixVersions`, 32,768), so a sync of
  79,000 pages pushes out pages, not spaces or sources, and a quiet space's
  count survives it.
- Each level costs a commit a little: a version per level above the named
  topic and a step up the hub's tree of watched topics. On 1,000 views a
  commit of two topics costs 150 ns at depth 1, 190 ns at depth 2 and
  220 ns at depth 3 (130 ns flat, before); a `:*` walks the views below
  it, about 50 µs for 1,000 (`BenchmarkChanged`, `BenchmarkChangedBelow`).

## Caching an aggregate against a version

The hub numbers the commits (`hub.Seq()`, 1, 2, 3 and on) and keeps, for each
topic, the number of the last commit that touched it (`hub.Version(topics...)`,
the highest of them). An expensive value, such as a count over 73,000 rows or
a sidebar tree, is good as of the number it was computed at, while the
version of the topics it read is not above that number.

```go
type count struct {
	n   int
	seq uint64 // the number it is good as of
}

type Counts struct {
	hub *live.Hub
	mu  sync.Mutex
	m   map[string]count // by space
}

// Items counts the items of a space, or takes the count from the cache
// while no commit touched the space's items since. seq is live.Seq(ctx)
// in a Render; a GET reads hub.Seq() before its View begins.
func (c *Counts) Items(ctx context.Context, q sqlite.Querier, space string, seq uint64) (int, error) {
	c.mu.Lock()
	e, ok := c.m[space]
	c.mu.Unlock()
	if ok && c.hub.Version("items:"+space) <= e.seq {
		return e.n, nil
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM items WHERE space = ?`, space).Scan(&n); err != nil {
		return 0, err
	}
	c.mu.Lock()
	if old, ok := c.m[space]; !ok || old.seq < seq {
		c.m[space] = count{n: n, seq: seq}
	}
	c.mu.Unlock()
	return n, nil
}
```

```go
n, err := counts.Items(ctx, q, space, live.Seq(ctx)) // in a Render

seq := s.hub.Seq() // in a GET: before the transaction begins
err := s.db.View(ctx, func(tx *sql.Tx) error {
	n, err := counts.Items(ctx, tx, space, seq)
	// ...
})
```

- The number comes from before the snapshot: every commit up to it is in
  the snapshot, and a later one may be too. At worst a value newer than its
  number is kept and computed again once; never an older one. A number read
  inside the transaction could claim a commit the snapshot does not hold.
- Families of topics make the cache last. Sync commands touch `sync` and
  `items:<space>` for each space they changed; clicks touch their tab and the
  item they changed. The sidebar tree, built from what sync writes, keys on
  `hub.Version("sync")`, so a click does not throw away what a sync built;
  search counts per source key on `hub.Version("sync:"+source)`. A value any
  write can change keys on `hub.Seq()`. Topics in a hierarchy are families
  already: `hub.Version("items:SWE")` moves with every `items:SWE:<id>`.
- The numbers start at 0 with each hub: keep such caches in memory, never in
  the database. `hub.MarkAll()` counts as a commit that touched every topic.
- The hub keeps the versions of at least 32,768 topics, those changed last,
  and at most twice as many (`live.Options.Versions`): about 70 bytes a topic
  with a name of 16, 4.3 MiB at most. A topic it no longer keeps reads as
  changed when it was pushed out, so a cache computes again and is never
  wrong. Topics per tab or per row churn through it; a cache keyed on them
  computes more often after a busy spell, nothing else.
- The levels above the named topics (`items:SWE`, `items`) have their own
  store, `live.Options.PrefixVersions`, 32,768 by default. A bulk sync that
  touches more distinct topics than `Versions`, one per item, pushes items
  out all along, and a cache keyed on an item computes again about once per
  32,768 new topics until it ends; it pushes out no space or source, so
  aggregates keyed on those survive it. Key aggregates on the level they
  cover. The second lever: raise `Options.Versions` above the distinct
  topics a sync touches, about 70 bytes a topic.
- `live.Cache` is a different thing: it keeps rendered fragments by their
  content. Use a version for the query behind a value, the cache for the
  HTML it renders to.

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

## Large updates and deletes

An UPDATE or DELETE over a large table (DeleteMissing after a full pass, a
one-time re-render that clears every cached rendering, an index rebuild)
holds the writer as long as it runs. Split it into commands by rowid range,
sent one after the other, so other commands get their turn between them.

```go
const chunk = 1000 // rows of about 4 KB: about 24 ms of writing each
var last int64
if err := db.R.QueryRowContext(ctx, `SELECT coalesce(max(id), 0) FROM comments`).Scan(&last); err != nil {
	return err
}
for lo := int64(1); lo <= last; lo += chunk {
	part := writer.Func(func(tx *writer.Tx) error {
		_, err := tx.Exec(`UPDATE comments SET html = NULL WHERE id BETWEEN ? AND ? AND html IS NOT NULL`, lo, lo+chunk-1)
		tx.Touch("comments") // the topics of what changed, as any command
		return err
	})
	if err := w.Do(ctx, part); err != nil {
		return err
	}
}
```

Measured on 2026-10-19 (i5-13500, NORMAL, 45,000 rows of 4 KB, 185 MB,
under 800 commands a second and a board rendered every tick;
`BenchmarkLargeWrite`, bench/RESULTS.md):

- One UPDATE of all rows: 0.49 s, then a 0.38 s checkpoint of the 186 MB
  it wrote to the WAL. Every other command waited up to 0.88 s (`Do`
  p50 632 ms), and the pages got no frame for as long: the hub keeps
  ticking, but without commits it has nothing to render. The WAL stays at
  186 MB until a second without commands.
- In chunks of 1,000 rows: 1.07 s in all, other commands at most 45 ms, a
  frame every tick (gaps of 51 ms), the WAL at 8 MB. Chunks of 5,000:
  0.89 s, commands up to 163 ms, frames 150 ms apart, the WAL at 21 to
  41 MB. Start at a chunk that writes for about one tick (50 ms) and size
  it by bytes, not rows: 1,000 rows of 4 KB is about 24 ms.
- With FULL, the same chunks cost a few milliseconds more per command.
- DELETE was cheap here: all 45,000 rows in 102 ms in one command, the WAL
  unchanged. Chunks of 5,000 kept other commands under 18 ms. Measure a
  delete on the app's own rows before chunking it.
- `w.Exclusive` with one transaction wrote the update in 0.34 s (likely
  because it runs no savepoint) and held everything for as long as one command, checkpoint
  included. It is right when the change must land whole and the pause is
  acceptable (a maintenance window, a deploy step), or for what a
  transaction cannot hold. It reaches no `OnCommit`: call `hub.Changed`
  or `hub.MarkAll` after it.
- An FTS5 `'rebuild'` of the 45,000 comments took 2.77 s in one statement,
  and the pages froze for 2.9 s. It takes no range; fill the index in
  rowid chunks instead if that pause is too long (not measured).
- Each chunk is a transaction of its own: a reader can see the table half
  done. A change that must not be seen half done is one command or one
  `Exclusive`.

## Work a transaction cannot hold

`PRAGMA foreign_keys` does nothing inside a transaction, and ATTACH, DETACH
and VACUUM are refused there. `w.Exclusive(ctx, fn)` runs fn on the writer's
own connection between two batches, outside any transaction; commands queued
meanwhile wait and run after it in order. It replaces a second write
connection, such as one kept for a factory reset or a tenant import: a
transaction on that one that runs past the busy timeout makes the
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

`lith/events` keeps the event log in the app's database. The writer serialises
commands, so there is no optimistic retry loop and no lagging read model: a
command calls `events.Execute` in its `Apply`, which reads the stream's state
row, checks the expected version, decides, appends, saves the row and runs the
inline projectors, all in the command's savepoint.

```go
type CardEvent interface{ cardEvent() }

type CardCreated struct {
	Title string `json:"title"`
}

type CardRenamed struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (CardCreated) cardEvent() {}
func (CardRenamed) cardEvent() {}

var Cards = events.NewStream[int64, CardEvent]("card")

// One line per stream: the compiler checks that T belongs to it.
func defineCard[T CardEvent](name string) events.Type[int64, CardEvent, T] {
	return events.Define(Cards, name, func(t T) CardEvent { return t })
}

var ( // the names are what the table stores
	Created = defineCard[CardCreated]("card.created")
	Renamed = defineCard[CardRenamed]("card.renamed")
)

type card struct{ Exists bool; Title string } // exported fields: kept as JSON

var rename = events.Decider[Rename, CardEvent, card]{
	Decide: func(c Rename, s card) ([]CardEvent, error) {
		if !s.Exists {
			return nil, ErrNotFound
		}
		if c.Title == s.Title {
			return nil, nil // nothing changes
		}
		return []CardEvent{CardRenamed{From: s.Title, To: c.Title}}, nil
	},
	Evolve:  evolveCard, // shared by the kind's deciders, one per command type
	Version: 1,          // bump when Evolve or the state change: rows refold
}

func (c Rename) Apply(tx *writer.Tx) error {
	return events.Execute(store, tx, rename, Cards.ID(c.Card), events.Exactly(c.Seen), c,
		events.Meta{Actor: c.Session, Causation: c.Op})
}
```

- The expected version is the one the page showed: `events.Exactly(v)`,
  `events.NoStream` for a stream that must not exist, `events.Any`. A mismatch
  fails with an error wrapping `events.ErrConflict` before `Decide` runs.
- Commands decide from the stream's state row, never from a projection.
- `events.Append(store, tx, id, want, meta, evs...)` for imports, without a
  decider; a single concrete event goes in as `events.One[CardEvent](e)`. It
  drops the stream's state row, so the next `Execute` refolds the stream.
- `Meta`: `At` (stored in Unix milliseconds, now when zero), `Actor`,
  `Correlation`, `Causation`.
- Test deciders with `eventstest.For(rename).Given(evs...).When(cmd).Then(t, evs...)`
  or `.ThenError(t, err)`; no database.
- A new optional field needs nothing. Any other shape change is a new type
  name plus `events.Upcast(old, new, fn)` passed to `events.New`; never edit
  stored events (`events.Compact`'s snapshots are the one exception). Keep a
  JSON sample per type in `testdata` and run `eventstest.Samples`.

## Projections of events

```go
// Every event type of the stream is handled or ignored (events.Ignore).
var cardTitles = events.NewProjector(Cards, "card_titles", 1,
	events.Handle(Created, func(tx *writer.Tx, r events.Record[int64, CardCreated]) error {
		_, err := tx.Exec(`INSERT INTO card_titles (id, title) VALUES (?, ?)`, r.ID, r.Data.Title)
		return err
	}),
	events.Handle(Renamed, func(tx *writer.Tx, r events.Record[int64, CardRenamed]) error {
		_, err := tx.Exec(`UPDATE card_titles SET title = ? WHERE id = ?`, r.Data.To, r.ID)
		tx.Touch(CardTopic(r.ID))
		return err
	}),
).Tables("card_titles") // created by a migration; written by this projector only

var store = events.Must(events.New(cardTitles))

runner := events.NewRunner(store, db, w, events.Options{})
go runner.Run(runCtx) // cancel runCtx before w.Close()
```

- Inline projectors (`Handle`) run in the appending command's transaction and
  touch the topics they change; `Execute` also touches `events.Topic`.
- Bumping a projector's version makes the Runner rebuild its `Tables` into
  copies (`card_titles__v2`), a command per `Options.Batch` events, and swap
  them in with the last batch; the batches run in `tx.Untouched`, so only
  the swap touches `cardTitles.Topic()`. Pages read the old table until the
  swap, and new events still reach it through the new handlers, in a
  savepoint whose failure is logged and leaves the command alone. No
  triggers on, and no foreign keys into, a projection's tables.
- After a rebuild an index `x` is `x__v2`: a migration that changes it runs
  `DROP INDEX IF EXISTS` on both names. The replaced table is dropped in
  one command, long for a large projection.
- Inline handlers only keep read models: a rebuild runs them again for
  every event, so a reaction (an event on another stream, a command, a
  call out) is a catch-up projector that sends its command with
  `writer.Once` keyed by `r.Position`.
- A projection table guards nothing: during a rebuild a `UNIQUE` on it fails
  only a logged handler on the old table, and the command goes on. A rule
  across streams (a unique slug) lives in a table no projector writes,
  written by the command itself.
- A projector's missing table fails `Run`, and in commands only `Execute`
  and `Append` on its own stream kind, naming the table.
- Catch-up projectors (`After`) run after the commit, outside the writer, for
  mail and other systems: at least once, with `r.Position` as the idempotency
  key. Failures back off (`Options.Backoff`, `MaxBackoff`); after `Retries`
  the projector parks with its error (`runner.Stats()`, `lith_projections`)
  until `runner.Resume(name)` or the next `Run`. `SkipHistory` starts a new one
  at the end of the log.
- Pages render from projections. `events.Load` (a stream) and `events.Since`
  (the log after a position) are iterators over any `sqlite.Querier`, for a
  history page, a feed near the end of the log, tests and tools.
- As of a position (a slider back in time): `events.LoadUntil`,
  `events.SinceUntil`, `events.StateAt` (a decider's state then), and
  `events.Bounds(ctx, q, Cards)` for the slider's ends (first and last
  position, 0 and 0 without events), never a query of the app's own on
  `lith_events`. A page of many entities reads a history projection whose
  rows carry `valid_from` and `valid_to` positions: `WHERE valid_from <= ?1
  AND (valid_to IS NULL OR valid_to > ?1)`. At 10,000 events: 0.46 ms a
  look, against 12.6 ms for a replay of the log (bench/RESULTS.md).
- Reads before the first command made the tables (`Load`, `LoadUntil`,
  `Since`, `SinceUntil`, `StateAt`, `Bounds`) find no events and no error:
  no `no such table` check in app code.
- Retention: `events.Forget(store, tx, id)` deletes a stream (erasure; delete
  its projection rows in the same command). `events.Prune(store, tx, Cards,
  events.Retention{Since: t, Newest: n, Limit: l})` deletes a kind's events
  that every rule set lets go, never past a catch-up projector; a rebuild
  then misses them. `events.Compact(store, tx, CardSnapshot, keep, decider,
  func(s card) CardSnapshot {...})` is Prune that keeps each stream's state:
  its last pruned event is rewritten in place (same position, version,
  metadata) as a snapshot event the app defines on the stream, which
  `Evolve` and every projector handle. The one deliberate rewrite of a
  stored event. Rebuilds make the same tables; as-of reads stay right from
  `Bounds`' first position (the newest snapshot) on. Positions are
  `AUTOINCREMENT` and never reused.

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

## Monitoring on a shared host

- Metrics on a loopback listener of their own, never the public one:
  `lith/prom` and `lith/dbmetrics/prom` under the app's namespace, with the
  Go and process collectors. The template serves them (`cmd/app/metrics.go`,
  `-metrics`: `auto` is 127.0.0.1 at `-addr`'s port + 100, `off` is none),
  and its unit passes `-metrics 127.0.0.1:8180` beside the socket, allows
  loopback (`IPAddressAllow=localhost`, so the app can reach the host's
  other loopback services too) and binds that port alone. Each app on a host
  needs a port of its own: the unit's `-metrics` and `SocketBindAllow` and
  `observability/<app>.metrics.yml` change together.
- The host's configs load folders, and each app installs its own files
  (`deploy/setup-host.sh`, never an edit of the host's files):
  `/etc/caddy/sites/<app>.caddy`; `/etc/prometheus/apps/<app>.yml`, the
  metrics target with the labels `app` (the unit's name) and `slot`
  (`prod`, `blue`, `green`, `staging`); `/etc/prometheus/targets/<app>.yml`,
  probes with `service` (the access log's `site`) and a generic `module`
  (`http_2xx`, `http_2xx_ok` for `/healthz`); `/etc/alloy/<app>.alloy`, the
  journal by `SYSLOG_IDENTIFIER` with slog's level as the label `level`, and
  the access log into the host's `loki.process.caddy_logs`.
- Alloy combines every file of `/etc/alloy` into one config: component
  labels start with the app's name (`app_` in the template, which lith-new
  rewrites).
- The "lith apps" dashboard and alert rules are lith's
  `deploy/observability`, installed by the host once. They match the metric
  names and `app`, never the scrape job, so a new app needs no edit there.

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
