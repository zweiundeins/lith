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
hub := live.New(live.Options{
	Read: db.R,
	// The streams' encoders and kept frames: set a Limit, or GOMEMLIMIT.
	// Without either the budget takes a quarter of the machine's memory.
	SSE: sse.Options{Budget: sse.NewBudget(sse.BudgetOptions{Limit: 224 << 20})},
})
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
// top serves h, the probes and the static files. ReadTimeout ends a body
// that does not arrive on every route, read or not, and spares streams.
srv := &http.Server{Handler: top, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 2 * time.Minute}
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
static, err := edge.NewStatic(files, "/static/", opts...) // mount at GET /static/, outside edge.Sessions
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
		return &writer.Refusal{Msg: "A move needs a card and a lane."} // the person reads it, inside an Act
	}
	return nil
}

func (c MoveCard) Apply(tx *writer.Tx) error {
	var board, version int64
	err := tx.QueryRow(`SELECT board_id, version FROM cards WHERE id = ?`, c.Card).Scan(&board, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return &writer.Refusal{Msg: "This card is gone."} // the page showed it: archived meanwhile
	}
	if err != nil {
		return err
	}
	if version != c.Seen {
		return &writer.Refusal{Msg: "Someone moved this card meanwhile."}
	}
	if _, err := tx.Exec(`UPDATE cards SET lane_id = ?, version = version + 1 WHERE id = ?`, c.Lane, c.Card); err != nil {
		return err
	}
	tx.Touch(fmt.Sprintf("board:%d", board))
	return nil
}
```

The handler hands the command over with `Do`, inside an `Act` that records
the op id under its key and any refusal in the tab's row (the board example's `Act`, which
also runs the command's `Validate` and records its message as a refusal),
and answers 204. The outcome reaches the page on its stream. A status code
carries nothing a person reads:

```go
func (s *Server) moveCard(w http.ResponseWriter, r *http.Request) {
	a, err := readMove(r) // the tab, the op key and id, and the move, from the signals
	if err != nil {
		w.WriteHeader(http.StatusBadRequest) // no page of ours sends it: a bug or a forged request
		return
	}
	act := Act{Session: a.Session, Tab: a.Tab, Key: a.Key, Op: a.Op, Cmd: a.Move, At: time.Now()}
	switch err := s.w.Do(r.Context(), writer.Once(a.Session+":"+a.Op, act)); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, writer.ErrClosed):
		w.WriteHeader(http.StatusServiceUnavailable) // the hub stopped first: Options.Stopping told the tab
	default:
		s.log.Error("move", "err", err)
		s.hub.Notify(streamID(a.Session, a.Tab), "notice:"+a.Key, notice(a.Key, a.Op, "That could not be saved. Try again in a moment."))
		w.WriteHeader(http.StatusInternalServerError)
	}
}
```

`notice` renders the message where the tab's outcome goes and patches the
ack signals as the tab's view does (`{"_ops": {key: {"ack": op,
"refused": true}}}`), so the pending state ends and the input stays. The stream ID names the
session and the tab (`streamID(sid, tab)`, `sid + "." + tab`), in the
stream's Spec, Watch, Unwatch and Notify alike: a bare tab id would let
any session that learns it push a notice with an ack it picks.

- `Do` waits for the commit and returns this command's error; `Send` returns
  at once (`ErrQueueFull` when the queue is full). A failed Send goes to
  `writer.Options.OnSendError(cmd, err)` on the writer's goroutine, or to the
  log without one.
- `Do` for what a person asked for: its 204 means stored, and a failed batch
  is known before the answer, so the handler can send the tab a notice and
  a pending button never waits for nothing. It costs about half a
  millisecond with NORMAL, and waiting handlers hold the load back. `Send`
  only for updates the next one replaces and nobody waits for (a pointer's
  position, a heartbeat): the site's cursor party.
- A status code carries nothing a person reads ("The 4xx and 5xx series are
  for machines and should never be seen by humans", Datastar's essay "I'm a
  Teapot"). Three cases:
  1. Outcome, the person caused it: a validation error, a refusal, a quota
     used up, a card that is gone or access lost meanwhile (an id from a
     page we rendered), a payment wall, a conflict with the tab's own
     earlier action, an upload's checks (too large, wrong type; uploads
     count though they go over plain fetch). The command records it in the
     tab's row (`Act` with a `*writer.Refusal`), the tab's view renders it
     with the op id, and the action answers 204.
  2. Notice, no command decided it: the writer failed, an abuse-sized rate
     limit, the server stopping. `hub.Notify(streamID, key, frame)` sends
     the frame once on the tab's open stream, after its views' frames,
     through `WriteTick`/`Write` like a view's frame; a later Notify with
     the key replaces one not sent yet; for a stream that is not open it
     does nothing and returns false (unlike `Watch`, nothing waits, so a
     made-up tab id costs nothing). Render it where the tab's outcome goes
     (the view's next frame puts the outcome back) and patch the op id into
     the ack like an outcome. Answer 500 or 429 with no body.
     `live.Options.Stopping(spec)` returns the events every stream sends
     when the hub stops, before `sse.Reconnect`: "Updating, back in a
     moment", which the next process's first frame of that view replaces.
     Best effort: a reconnect or a reload loses a notice.
  3. Only the browser knows: no answer (Datastar's `retries-failed`), the
     stream down. The page says so in its own words, never from a body.
  The pending state ends on a signal, never on a clock: the op's ack (an
  outcome or a notice), `retries-failed`, a 4xx other than 409 and 429 (no notice
  comes with it), or the stream connecting again with a tab row that does
  not ack the op (a notice a reconnect lost, or one that found no stream).
  The page tracks ops in one pattern (the board example's `expr.go`):
  - Op keys: what the person acted on, `add` for a form, `card-17` for a
    card's button. `$_ops[key]` = `{op, ack, refused, inflight, recheck}`.
    The tab's row keeps each key's newest op and whether it was refused
    (`TabState.Ops`, pruned to 16 keys); the tab view patches
    `{"_ops": {...}, "_synced": true}` (an empty map, never null, which
    would delete the signal); a notice patches its key's ack, with the
    notice key `notice:<key>`.
  - One stable element, `<div id="ops" hidden>`, outside every region a
    frame morphs, holds the listeners. `act(key, path, payload)` makes the
    op id and appends `<i data-op-key=key data-init="@post(path, {headers:
    {'Tab-Id', 'Op-Key', 'Op-Id'}, payload, requestCancellation:
    'disabled'})">` to it. The tab and op in headers let the handler check
    its rate limit and notify before it reads the body; the handler reads
    them from the headers only (a copy in the payload may stay for tests
    and tools that post plain JSON, unread). Headers are safe
    here (each op's `<i>` runs its data-init once, with that op's values,
    and a retry should repeat them), not on the stream, whose reopens
    would repeat stale ones. The element lets the request outlive
    the form, dialog or card it came from (a form in a closed modal never
    leaks the count or misses its end). `requestCancellation: 'disabled'`:
    Datastar 1.0.4 cancels page-wide by method and URL, so two cards' moves
    to `/cmd/move` would abort each other.
  - `fetched`, `data-on:datastar-fetch` on `#ops`, matches
    `evt.detail.el.dataset.opKey`: `started` raises the key's `inflight`,
    `finished` lowers it (clamped at 0), removes the element, and with
    `inflight` 0 and `recheck` set reopens the stream if the op still
    waits; `retries-failed` ends a waiting op ("The connection is down");
    `error` with a status that is not 409 or 429 and below 500 ends a
    waiting op ("That did not work"): handlers send a notice with every 429
    and 5xx, a 409 (no stream for the tab) waits for the reconnect,
    so only those wait for the stream.
  - `patched`, `data-on-signal-patch` on `#ops` with
    `data-on-signal-patch-filter="{include: /^(_ops\.[^.]+\.ack|_synced)$/}"`
    (an effect would fire on the click), wrapped in one `(() => {...})()`
    since Datastar splits that attribute at semicolons: a changed ack
    equal to the op settles it (and empties the input unless refused);
    `patch._synced` checks every waiting key: with a request in flight it
    sets that key's `recheck`, else it ends the op ("That did not go
    through") and keeps the input.
  - In-flight and recheck are per key: a hung request holds only its own
    key, and a lost op elsewhere still ends on the next reconnect.
  - `data-on-signal-patch` fires only on a change, so every open, retry
    and hide must set `$_synced` false, or the next `true` changes
    nothing and `patched` never sees the reconnect.
    Opening the stream sets `$_synced = false`, and so do `retrying` on the
    stream element and a tab that hides
    (`data-on:visibilitychange__window="document.hidden && ($_synced = false)"`:
    Datastar closes a hidden tab's `@get` and opens it again on show,
    without data-init or `retrying`). Never end an op while its request is
    open: its command may still wait in the writer, and "did not go
    through" then the commit means a retry with a new op id and a
    duplicate. The reopen after the last request decides: a 204 means
    committed, so the fresh frame holds the ack (a replacing stream renders
    the views only its ID watches afresh, which covers a blue/green deploy
    whose draining old process commits where the new hub never hears of
    it); a failure status means a notice went out, perhaps to a stream
    that was gone, and the fresh frame ends the op.
  - The toast or message for an op's end lives in `patched` on `#ops` too:
    the place the person acted may be gone (feedback rule 4).
  - A form posted with `contentType: 'form'` needs `novalidate` and its
    own checks in the expression: Datastar runs `checkValidity` after
    `started` and never sends `finished` when it fails.
  - Every check a person can fail is a refusal in `Apply` (or a `Validate`
    run inside `Act`); a bare 400 leaves the person only "That did not
    work".
  A command that runs long (an import) first records its acceptance in the
  tab's row ("Import started"), which acks the op at once, and reports
  progress from state; pending means "taken", not "done".
  Bare statuses for machines: a body that does not parse or an id no page
  holds gets 400 (a bug or an attack), a 503 when the writer is closed (the
  hub stopped first, `Stopping` told the tab).
- `Notify` details: a notice waits for the renders pending for the
  stream's views when it is called (the commit just before it), and goes
  after their frames in the same write; it waits behind a Slow stream's
  250 ms and a paced stream's full window (up to `live.AckTimeout`, 10 s).
  One slot per key, last-wins, keeping the first one's place. Key notices
  as the page acks: a page that acks per op key (`$_ops[key]`, one op
  pending per key) uses the op key; a page that acks per op
  (`$_outcomes[op]`) keys by the op id, or a later notice drops an earlier
  op's, which stays pending until a reconnect.
  At most `live.MaxNotices` (64) wait per stream; a new key beyond them
  drops the oldest. Keys from the client (op keys) are checked by the
  handler, and an action without an op uses one fixed notice key. A
  pending render that fails holds a notice no longer than its tick: on a
  failing database no further commit may come to render it again.
  `[]byte` notices still waiting when the hub stops go out before
  `Stopping`'s events. `Stopping` runs on every stream's goroutine at
  once; a panic in it is logged and costs that stream its events.
- A check the handler makes before any command (the account's plan, a
  payment wall) still records its refusal through an `Act`-style command,
  so the tab's view shows it.
- Size a rate limit so that a person using the page never reaches it (429
  plus a notice). A limit intended use can reach is a product rule: a
  command that counts and refuses, a quota ("You have used this month's 20
  scans").

### Uploads

A file goes over plain `fetch`, and the rules hold:

- The tab and the op travel in the query string or in headers: a body over
  `http.MaxBytesReader`'s limit cannot be parsed for them.
- The handler checks the file (too large, wrong type) and records the
  outcome with a command, as `Act` records a refusal, before it answers;
  the tab's view brings the ack and the reason.
- A plain `fetch` sends no `datastar-fetch` events: the page's expression
  sets the key's entry as `act` does and raises its `inflight` before the
  request, ends the op itself on a 4xx other than 409 and 429, sets `recheck` when
  the `fetch` rejects, and in `finally` lowers `inflight` and, at 0 with
  `recheck` set, reopens the stream if the op still waits (pages.md,
  Uploads).
- Check the size in the page first. A handler that answers "too large"
  before reading the body makes Go close the connection after about 256 KB
  unread, and Chrome often rejects the `fetch` with
  `ERR_CONNECTION_RESET` though the outcome was recorded: treat a rejection
  as a lost answer (the stream or the reopen decides), not as "the
  connection is down" while the stream is fine.
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

## In-process listeners

`tx.Publish(v)` hands a value to the writer's subscribers once the batch
commits; a failed command, or a rolled-back savepoint, drops it like an
`AfterCommit` function. `w.Subscribe[T](ctx, n)` returns a channel of the
values whose dynamic type is `T` (`any` for all, an interface for its
implementers; a `*Card` reaches no `Subscribe[Card]`), in commit order,
queued after `AfterCommit` and `OnCommit` (even when they panic) and before
`Do` returns; queued, not handled. The channel closes when ctx ends or the
writer stops. Subscribers share each value: publish values nobody changes.

It is best effort: each subscriber has a queue of n, and a full queue drops
the value for that subscriber (`Stats.Dropped`,
`<ns>_writer_publish_dropped_total`); a crash between commit and delivery
loses it; a subscription gets what is delivered while it is open; nothing
is replayed. `Stats.Subscribers` (`<ns>_writer_subscribers`) counts the
subscriptions open: one that only grows has a ctx that never ends.
`w.Exclusive` runs no command and publishes nothing: follow it with a `Do`
that only publishes.

Pages need none of it. Views render from the database and `tx.Touch`
renders every view that shows the change. A passing effect (a pulse on a
card just edited) comes from data too: the row's edited-at and edited-by,
and the view adds a class while the edit is recent. A tab's outcome (saved,
a validation error, a refused move) is tab state, written by the command,
so it survives a reconnect and a reload.

Pick by who must hear of the change and what may miss it:

- `tx.Touch`: views. Nothing misses it; views read the database.
- `tx.AfterCommit`: a side effect of this one command (wake the outbox's
  sender). Keep the work in a table, so a crash before `fn` loses nothing.
- `tx.Publish`: in-process listeners outside the views (notifications and
  mail, achievements, an admin firehose, metrics, caches, webhooks). Each
  may miss a value, so one that must be right reconciles from the data.
- `lith/events`: a durable log with projectors that catch up after a
  restart.

A listener with a reconcile counts through the writer, never beside it: a
count read on `db.R` can already hold a change whose value is still on its
way, which the listener then applies twice. The count is a command that
reads in the writer's transaction and publishes a snapshot, so it arrives
in commit order with the deltas. The listener subscribes first, sends the
count, replaces its state on a snapshot and adds the deltas after it:

```go
type CountOpen struct{}

func (CountOpen) Apply(tx *writer.Tx) error {
	open, err := OpenCards(tx.Context(), tx)
	if err != nil {
		return err
	}
	tx.Publish(OpenCount(open)) // a snapshot: every assignment before it, none after
	return nil
}

// changes := w.Subscribe[WorkloadChange](ctx, 256), then w.Send(CountOpen{})
// at once and every interval, and for each change:
func (l *Workload) apply(c WorkloadChange) {
	l.mu.Lock() // Open reads l.open from other goroutines
	defer l.mu.Unlock()
	switch c := c.(type) {
	case OpenCount:
		l.open = maps.Clone(c) // shared with every subscriber
	case Assigned:
		if l.open == nil {
			return // the first count, still to come, holds it
		}
		l.open[c.From]--
		l.open[c.To]++
		delete(l.open, "") // "" is nobody: the From of a card nobody had, the To of one unassigned
	}
}
```

Between counts it is exact unless its queue dropped a value or a change
published none; the next count puts both right. The count runs inside a
writer batch, so every other write waits for it: keep it cheap (an index
covers it) or infrequent.

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
			// templ writes into the event, in a buffer the size of the last frame.
			e := sse.NewElements(live.FrameBuffer(ctx), sse.Patch{Selector: "#board"})
			if err := views.Board(b).Render(ctx, e); err != nil {
				return nil, err
			}
			return e.Event(), nil
		},
	}
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	tab := readTabID(r)
	s.hub.Serve(w, r, live.Spec{ID: streamID(sid, tab), Client: edge.ClientKey(r), Views: []live.View{boardView(id), s.tabView(sid, tab)}, Reconnect: "stream"})
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
  order they opened. A stream's ID is its session's and tab's: render a
  hash of it, never the ID, since whoever knows it can open a stream that
  ends the tab's own.
- `Spec.Write` tailors a shared frame per stream (kanban sends only the lanes
  a stream has not seen). Keep it free of formatting: format once in Render.
  `Spec.WriteTick(s, frames)` gets all of a stream's new frames at once, in
  the order it watches the views: one `s.Write(events...)` of them is one
  compressed write and flush per tick instead of one per view.
- A page's command to watch a view (a panel that scrolls into view) can
  arrive before its stream. `hub.Watch(streamID(sid, tab), v)` then keeps v for
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
  element) to `POST /pace`, which validates the tab id, limits each client
  address (`edge.NewLimiter(4, 20)`), then each session
  (`edge.NewLimiter(1, 5)`), calls `hub.Behind(id)` and
  `hub.Pace(id, 1)`, and answers 204: the hybrid in place, the default.
  Every stream has `Spec.Ack` and no `Window`, so it starts unpaced;
  `Release` (5 s) and `ReleaseSlack` (100 ms) stay at their defaults. Each
  paced write ends with an `<i>` patched inner into the stream's element
  whose `data-init` posts `POST /ack?tab=&s=&n=&rtt=` after the paint, the
  round trip from Resource Timing; `/ack` validates, limits each client
  address (`edge.NewLimiter(80, 160)`), then each stream
  (`edge.NewLimiter(20, 40)`), calls `hub.RoundTrip` then `hub.Ack`, and
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
	v := signedOut // a sign-in link in the list's place
	if person, ok := s.person(r); ok { // from the session, never from the URL or a signal
		v = noSpace // "No such space." in the list's place
		if space, ok := spaceID(r); ok {
			v = pagesView(person, space, r.URL.Query().Get("q")) // renders noSpace for an id with no row
		}
	}
	sid := edge.SessionID(r.Context())
	s.hub.Serve(w, r, live.Spec{ID: streamID(sid, readTabID(r)), Views: []live.View{v}})
}
```

- Signed out, or a space that does not exist: the stream opens all the
  same and a view says so. A 401 or 404 shows the page nothing, and
  `retry: 'error'` asks again every 10 s for ever; a bare 204 ends the
  retries but leaves the region empty.
- The stream's ID names the session too (`streamID(sid, tab)`), as every
  stream's does: a bare tab id lets whoever learns it send that tab
  notices and views.

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

## Refreshing a page after a deploy

After a deploy the stream reconnects and its views render afresh; the rest
of the page is the old process's HTML until a reload. `Spec.Refresh` fixes
that as the stream opens.

The page starts with `_build` (from `hub.Build()`, which is
`live.Options.Build`, by default the binary's VCS revision) and `_dirty`
(true on input in a form, false on submit), and `_assets` (the hash of its
scripts and stylesheets). Its stream reports them as signals and its own
path in a header. Not the other way round: Datastar 1.0.4 builds a `@get`'s
headers once, and its own reopens (a retry, a hidden tab shown again)
repeat them while they read the signals afresh, so a header build refreshes
the tab on every show after a deploy and a header typed flag stays false.
The `exclude` is needed: its default drops every `_` signal. Datastar keys
cancellation by the URL written here, without the signals, so a reopen still
aborts the old request.

```js
@get('/stream?tab=' + $_tab, {headers: {'Page-Path': location.pathname + location.search}, filterSignals: {include: /^_(build|assets|dirty)$/, exclude: /(?!)/}, retry: 'error', retryMaxCount: Infinity})
```

```go
var page struct {
	Build  string `json:"_build"`
	Assets string `json:"_assets"`
	Dirty  bool   `json:"_dirty"`
}
if sse.ReadSignals(w, r, &page) != nil { // a GET's come in its datastar query parameter
	w.WriteHeader(http.StatusBadRequest)
	return
}
spec := live.Spec{
	ID: streamID(sid, tab), Client: edge.ClientKey(r), Views: views, Reconnect: "stream",
	Build: page.Build, Page: r.Header.Get("Page-Path"), Dirty: page.Dirty,
	Refresh: func(ctx context.Context, rq live.RefreshRequest) ([][]byte, error) {
		if rq.Dirty || page.Assets != assets {
			return [][]byte{reloadNotice}, nil // a morph would take the input, or miss the new CSS and JS
		}
		html, err := renderContent(ctx, rq.Page) // <div id="content">, as the GET renders it
		if err != nil || html == nil {
			return nil, err // nil: the page is current
		}
		return [][]byte{sse.PatchElements(html)}, nil
	},
}
```

- It runs only when the reported build is set and differs from the hub's.
  Its events are the stream's first write, the views' first frames follow
  (and win), then lith patches `_build` (`Options.BuildSignal`) in a
  separate signal patch, also after nil, so the next reconnect costs nothing.
- Morph a container the app names, never `<body>`. The stream's element
  (`Spec.Reconnect`), `#ops` and the signals stay outside it: a morphed
  stream element opens a second stream, so lith skips such a refresh
  (`Stats.RefreshesGuarded`).
- Signals in refreshed markup use `data-signals:x__ifmissing`. Elements with
  `data-init` or `data-on-intersect`, and stateful custom elements, keep
  stable ids and unchanged attributes, sit in a live view, or carry
  `data-ignore-morph`; signal-driven attributes need `data-preserve-attr`.
- Return nil for a page the deploy did not change (compare its ETag): its
  tabs pay only the check.
- A `Refresh` that only ever returns the reload notice, no morph, is a
  supported choice, not only the dirty-page fallback: for areas that keep
  state in plain signals and stateful elements (an editor, a board).
- The default build is the same for every local build of a modified tree
  (`<rev>+dirty`), so local tests never refresh. Set `Options.Build` to
  something content-based, such as a hash over all pages' ETags: that also
  makes a deploy that changes no page send nothing.
- A live region inside the morphed container shows its placeholder for an
  instant before its frame follows: harmless.
- At most `Options.RefreshConcurrency` (default `Workers`) run at once, each
  under `RenderTimeout`, and one at a time per `Spec.Client` (the build is
  the client's to forge, so set `Client`). One that fails, times out or
  waits longer than `live.RefreshWait` (1 s) for a slot is skipped and
  logged, and the build signal stays, so the next reconnect tries again.
  Metrics: `<ns>_live_refreshes_total`, `_refreshes_unchanged_total`,
  `_refreshes_skipped_total{reason}`.
- Share the container's render among the tabs of one page for a second
  (the template's `shared`, keyed by page, plus language where there is
  one): after a deploy every tab refreshes at once, and a render per tab
  is O(tabs) CPU and garbage.
- No stream, no refresh: a page that needs it opens a minimal stream (an
  idle stream that hibernates is cheap).

## Which feedback to give

Mechanisms: the re-render; an inline error (the form's error slot, or the
field); standing state (a banner, badge or status rendered from data while
the condition lasts); a toast (does not block, goes away by itself, one
action at most); progress (work outside a request); a dialog (confirm,
prompt, choose). The first rule that fits decides:

1. The change shows where the person acted: nothing more. No toast that
   repeats the re-render.
2. The person must fix something (a refusal, an input check): inline,
   where they acted. Never a toast, never a dialog.
3. Something holds until they act (a limit reached, a backup unhealthy):
   standing state from data. Never a toast.
4. It happened where they cannot see it (the place they acted is gone,
   another page, the item left the list, a long command accepted with
   progress from state): a toast.
5. An offer or a celebration: a toast with its one action; missing it does
   no harm.
6. A notice, not caused by them (writer failure, rate limit, restart): a
   toast at warning or error level, in the page's words, never a body.
7. Only the browser knows (no connection, "That did not go through"): a
   toast in the page's own words.
8. A dialog only to ask, never to report.

Delivery: outcomes (2, 4, 5) are written to the tab's row by the command
and rendered by the tab's view; notices (6) go on the open stream with
`hub.Notify`, best effort; rule 7 the page says itself; a status code
never carries text for a person. A toast for an outcome shows once per op:
the tab's view renders the outcome's text with the ack, and the page shows
the toast in `patched` on the stable `#ops`, when it first sees that op's
ack while the op is pending; the place the person acted may be gone by
then (rule 4). A reconnect renders the row again, but the op is settled; after a
reload no op is pending, and the outcome renders as state. The board:
card added is rule 1, the refusal in `#outcome` rule 2, "The connection is
down" rule 7, a rate limit's or failed write's notice rule 6. Without
toasts, an inline line stands in for one: `$_problem` under the form, with
`role="alert"`.

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
			e := sse.NewElements(live.FrameBuffer(ctx), sse.Patch{Selector: "#main"})
			if err := page.Render(ctx, e); err != nil {
				return nil, err
			}
			return e.Event(), nil
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
  per topic, about 2 µs per view for four topics in and four out
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

Measured on 2026-10-19 and 2026-10-20 (i5-13500, NORMAL, 45,000 rows of 4 KB, 185 MB,
under 800 commands a second and a board rendered every tick;
`BenchmarkLargeWrite`, bench/RESULTS.md):

- One UPDATE of all rows: 0.33 s (0.47 s up to v0.4.0, in a savepoint),
  then a 0.36 s checkpoint of the 186 MB it wrote to the WAL. Every other
  command waited up to 0.68 s (`Do` p50 520 ms), and the pages got no
  frame for as long: the hub keeps
  ticking, but without commits it has nothing to render. The WAL stays at
  186 MB until a second without commands.
- In chunks of 1,000 rows: 1.07 s in all, other commands at most 45 ms, a
  frame every tick (gaps of 51 ms), the WAL at 8 MB. Chunks of 5,000:
  0.89 s, commands up to 163 ms, frames 150 ms apart, the WAL at 21 to
  41 MB. Start at a chunk that writes for about one tick (50 ms) and size
  it by bytes, not rows: 1,000 rows of 4 KB is about 24 ms.
- With FULL, the same chunks cost a few milliseconds more per command.
- `writer.Options.CheckpointAside` checkpoints on a connection of its
  own, beside the batches: after the one UPDATE other commands waited at
  most 0.34 s, its own time, and in chunks of 1,000 the update took
  0.73 s, the WAL up to 15 MB. Off by default.
- DELETE was cheap here: all 45,000 rows in 102 ms in one command, the WAL
  unchanged. Chunks of 5,000 kept other commands under 18 ms. Measure a
  delete on the app's own rows before chunking it.
- `w.Exclusive` with one transaction wrote the update in 0.34 s, as fast
  as a command alone in its batch, which runs no savepoint either, and
  held everything for as long as one command, checkpoint included. It is right when the change must land whole and the pause is
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
- A bound per stream for `Compact` (each document's oldest commit that no
  saved version holds): a table `(kind, id, at)`, primary key `(kind, id)`,
  index on `(kind, at)`. Appending inserts the stream's row if none
  (`INSERT OR IGNORE`, the event's time); moving the bound deletes it and
  inserts the first event past the bound from `lith_events`. The sweep
  passes `SELECT min(at) FROM uncut WHERE kind = ?` (one index step, no scan
  per stream) as `Since`, with a `Limit`, again while it deletes. Compact's
  marks keep each call short while the bound only moves on. Move every
  stream's bound on a schedule: one that never moves holds all back.

## Backups

- Without Litestream: `db.Backup(ctx, path)` (VACUUM INTO on its own
  read-only connection) from a timer, rotated, copied off the machine.
- With Litestream: the nested module `github.com/zweiundeins/lith/backup`
  restores a missing file before `sqlite.Open`, attaches, verifies a restore
  daily; open the database with `sqlite.CheckpointExternal` so Litestream
  owns the checkpoints.
- A provider incident fails every replica's verification at once, and
  usually passes. `RunVerifyLoop(ctx, backup.VerifyOptions{OnFail: ...})`
  retries the failed replicas 10 and 30 minutes later and calls `OnFail`
  once per run with those that failed every attempt
  (`[]backup.VerifyFailure`: `Name`, `First`, `Last`, `Attempts`). Alert
  there, once per run, never per replica; passes on a retry show in
  `<ns>_backup_replica_verify_retried_total{replica}`, not in an alert.
  For incidents that last hours, stretch `Retries` toward the interval
  (`30m, 2h, 6h` against 24h).
- Both loops cover attached replicas. Tenants attached on demand: pass the
  account list as `VerifyOptions.Names` (a `Names` error is one `OnFail`
  failure with an empty `Name`) and as `MaintenanceOptions.Names`. The
  names not attached get a loop of their own, a pass a day
  (`DetachedInterval`), one name at a time with 250 ms after each
  (`DetachedConcurrency`, `DetachedPause`), since each name costs a client
  and a TLS handshake; no snapshot, only pruning through the bucket.
  Attached replicas keep their hourly pass (2 at a time, 100 ms). A due
  snapshot runs in the background after `Attach` (`Config.SnapshotTimeout`,
  10 min); detach cancels it and the hourly pass's work on the tenant, so
  an eviction or a shutdown waits for a cancel and, on S3, the abort of
  the cut-off upload (10 s at most), not an upload. A pass that finds the
  tenant busy skips it (`_replica_maintenance_skipped_total`).
  `PurgeReplica` holds the name and aborts unfinished multipart uploads too
  (their parts are not objects; a provider answering `NotImplemented` has
  none to abort); add a lifecycle rule `AbortIncompleteMultipartUpload` as a
  backstop where the provider supports it (unconfirmed on Swift-based S3
  such as Infomaniak). Failures of a pass log one line; the
  metrics are `<ns>_backup_detached_maintenance_passes_total`,
  `_failures_total`, `_detached_pruned_objects_total` (approximate) and
  `_detached_last_pass_timestamp_seconds`, without a replica label. Alert
  when the last is older than two intervals `and ... > 0` (0 until the
  first pass); it is set even when every name failed, which the failures
  counter shows.
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
