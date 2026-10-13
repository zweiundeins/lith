---
title: Commands and the writer
nav: Commands
summary: Every change is a command that the one writer applies in a batch. How to write a command, hand it over, refuse, survive a retry and act after the commit.
order: 1
---

The writer owns the database's only write connection, and every change to the database is a command that it applies. [Linear writes](/docs/concepts/linear-writes) explains why; this page shows how. The examples are the board app's commands, in `site/internal/docs/example/board`.

## A command

A command is a value with an `Apply(*writer.Tx) error` method. `Apply` runs inside the batch's transaction, in a savepoint of its own, and reads and writes through the `*writer.Tx` it gets:

```go source=site/internal/docs/example/board/commands.go
// AddCard adds a card at the end of a lane.
type AddCard struct {
	Lane  int64
	Title string
}

// Validate runs before the command is queued: Send and Do return its
// error wrapped in writer.ErrInvalid, and the writer never sees it.
func (c AddCard) Validate() error {
	if c.Lane <= 0 {
		return errors.New("a card needs a lane")
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(c.Title)); n == 0 || n > MaxTitle {
		return fmt.Errorf("a title has 1 to %d characters", MaxTitle)
	}
	return nil
}

func (c AddCard) Apply(tx *writer.Tx) error {
	_, err := c.Insert(tx)
	return err
}

// Insert adds the card and returns its id. A full lane refuses it.
func (c AddCard) Insert(tx *writer.Tx) (int64, error) {
	var board int64
	var limit int
	err := tx.QueryRow(`SELECT board_id, max_cards FROM lanes WHERE id = ?`, c.Lane).Scan(&board, &limit)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	n, err := LaneCount(tx.Context(), tx, c.Lane) // the same query a GET and a view run
	if err != nil {
		return 0, err
	}
	if n >= limit {
		return 0, &writer.Refusal{Msg: "This lane is full."}
	}
	res, err := tx.Exec(`INSERT INTO cards (lane_id, title) VALUES (?, ?)`, c.Lane, strings.TrimSpace(c.Title))
	if err != nil {
		return 0, err
	}
	tx.Touch(Topic(board))
	return res.LastInsertId()
}
```

Three parts of it carry the rules:

- `Validate` checks what the command can check alone, before it is queued. `Send` and `Do` call it, when the command has one, and return its error wrapped in `writer.ErrInvalid`.
- The check that reads before it writes, whether the lane has room, is inside `Apply`. The command sees every command queued before it, and nothing changes between its read and its write, so there is no retry loop.
- `tx.Touch(Topic(board))` names what changed. After the batch commits, the writer hands the touched topics to `Options.OnCommit`, which is `hub.Changed`, and every view that lists one of them renders again in the next tick. A command that touches nothing leaves every page as it was.

An error from `Apply`, or a panic, rolls back the command's savepoint: its changes, its topics and its `AfterCommit` functions go, and the other commands of the batch commit without it. A panic comes back as an error that wraps `writer.ErrPanic`, and the log gets its stack.

For a one-off change, `writer.Func` makes a command of a function:

```go source=site/internal/docs/example/board/outbox.go
		sent := writer.Func(func(tx *writer.Tx) error {
			_, err := tx.Exec(`DELETE FROM outbox WHERE id = ?`, m.id)
			return err
		})
```

## Handing a command to the writer

There are three ways to hand a command over. All of them queue it, and the writer applies the queue in the order it arrived.

| Call | Returns | For |
| --- | --- | --- |
| `w.Do(ctx, cmd)` | the command's own error, once its batch has committed | handlers: the answer waits for the commit |
| `w.Send(cmd)` | at once; `ErrQueueFull` when the queue has no room | work nobody waits for |
| `w.DoValue(ctx, fn)` | what `fn` returned, once its batch has committed | a new row's id, for a test or a tool |

`Do` waits for room in the queue until `ctx` ends. Once the command is queued it always runs, and `Do` reports its outcome even if `ctx` ends meanwhile: a handler never mistakes a committed change for a failed one. `tx.Context()` carries the values of `Do`'s context but is never cancelled, so a statement cannot stop halfway through a batch.

`Send` returns before the command runs, so its error goes somewhere else: to `Options.OnSendError(cmd, err)` when the app sets one, on the writer's goroutine after the batch, and to the log otherwise.

`DoValue` takes a function, not a command, so a command's `Validate` does not run unless the caller calls it:

```go source=site/internal/docs/example/board/commands.go
// Create creates a board and returns its id, for a test, a tool or a new
// account. DoValue takes a function, not a command, so the command's
// Validate does not run unless Create calls it.
func Create(ctx context.Context, w *writer.Writer, name string) (int64, error) {
	c := CreateBoard{Name: name}
	if err := c.Validate(); err != nil {
		return 0, err
	}
	return w.DoValue(ctx, c.Insert)
}
```

## The handler's answer

A handler validates, hands the command over and answers 204. The page learns the outcome from its stream, as every other page that shows the board does. `Do`'s error says which answer to give:

```go source=site/internal/docs/example/web/server.go
	switch err := s.w.Do(r.Context(), writer.Once(sid+":"+a.Op, act)); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, writer.ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, board.ErrNotFound):
		http.Error(w, "no such card or lane", http.StatusNotFound)
	case errors.Is(err, writer.ErrClosed):
		http.Error(w, "the server is restarting", http.StatusServiceUnavailable)
	default:
		s.fail(w, r, err)
	}
```

`writer.ErrClosed` comes once the writer has stopped taking commands. Run the writer on a context that outlives the HTTP server and call `w.Close()` after `srv.Shutdown`, so the actions that arrive while the server drains still commit ([Wiring](/docs/getting-started/wiring#starting-and-stopping) has the order). A batch that fails as a whole, at its `BEGIN`, a savepoint or its `COMMIT`, fails every command in it that had not failed on its own, with the batch's error.

## Order and Flush

Commands apply in the order they arrive, from `Send` and `Do` alike. `w.Flush(ctx)` returns once every command queued before it has been applied, so a test can send, flush and read:

```go source=site/internal/docs/example/board/board_test.go
func TestFlush(t *testing.T) {
	db, w, _ := open(t)
	ctx := context.Background()
	for range 3 {
		if err := w.Send(board.AddCard{Lane: 1, Title: "sent"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(load(t, db).Lanes[0].Cards); n != 3 {
		t.Errorf("%d cards after Flush", n)
	}
}
```

## Refusals

A refusal is an outcome the user reads, such as "This lane is full." Return it as a `*writer.Refusal` with the message. Like any error it undoes the command's changes:

```go source=site/internal/docs/example/board/commands.go
	if version != c.Seen {
		return &writer.Refusal{Msg: "Someone moved this card meanwhile."}
	}
```

A page shows a refusal next to where the user acted, and it learns of it the way it learns of everything else: from its stream. So the board's actions run inside `Act`, which records the outcome in the tab's row. `tx.Savepoint` runs the action's command in a nested savepoint: a refusal undoes the command's changes and drops its topics, and `Act` writes the tab's row all the same.

```go source=site/internal/docs/example/board/commands.go
func (a Act) Apply(tx *writer.Tx) error {
	state, err := Tab(tx.Context(), tx, a.Session, a.Tab)
	if err != nil {
		return err
	}
	state.Op, state.Refusal = a.Op, ""
	var refusal *writer.Refusal
	switch err := tx.Savepoint(a.Cmd.Apply); {
	case errors.As(err, &refusal):
		state.Refusal = refusal.Msg
	case err != nil:
		return err
	}
	return saveTab(tx, a.Session, a.Tab, state, a.At)
}
```

The tab's view renders the row, and [Pages and Datastar](/docs/guides/pages#op-ids-and-the-pending-state) shows how the page waits for it.

## Op ids and retries

A network repeats requests: a proxy retries, a user clicks twice, Datastar retries an action whose answer got lost. Each action carries an op id that the page made, and the handler wraps its command in `writer.Once` with a key of the session and the op:

```go source=site/internal/docs/example/web/server.go
	act := board.Act{Session: sid, Tab: a.Tab, Op: a.Op, Cmd: cmd, At: time.Now()}
	switch err := s.w.Do(r.Context(), writer.Once(sid+":"+a.Op, act)); {
```

When the command succeeds, `Once` records the key in the table `lith_ops`, in the same savepoint. A command whose key is recorded succeeds without running again. A failed command recorded nothing, so a retry runs it again and gets its error afresh. `Act` turns a refusal into a success that wrote the tab's row, so a retry of a refused action replays nothing either.

`writer.OnceResult` keeps an answer as well, for an action whose response carries one. Creating a board answers with the new board's address, and a retry must get the same address, not a second board:

```go source=site/internal/docs/example/web/server.go
	op := writer.OnceResult(edge.SessionID(r.Context())+":"+req.Op, board.CreateBoard{Name: req.Name})
	switch err := s.w.Do(r.Context(), op); {
	case errors.Is(err, writer.ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
```

The command is a `writer.ResultCommand`: its `ApplyResult` returns a few bytes, here the id. `OnceResult` records them in `lith_results` with the key, and also records a refusal with its message: a retry of a refused command gets the same `*writer.Refusal` without running. After `Do`, `op.Result()` holds the bytes, from this run or the recorded one, and `op.Replayed()` says which. Other errors record nothing.

Both tables grow with every action. `writer.PruneOnce(24 * time.Hour)` forgets the keys older than a day; send it from the hourly sweep ([Migrations, jobs and imports](/docs/guides/migrations-and-jobs#sweeps)).

## Work after the commit

A command never waits for the network: every command queued behind it waits while it runs. Mail, webhooks and calls to other services happen after the commit, outside the writer. `tx.AfterCommit(fn)` runs `fn` on the writer's goroutine once the batch has committed, before `Do` returns, and drops it if the command fails. `fn` must not block either.

The board writes the mail into an outbox table in the command's own transaction, so the mail exists if and only if the change committed, and `AfterCommit` only wakes the sender:

```go source=site/internal/docs/example/board/commands.go
	if _, err := tx.Exec(`INSERT INTO outbox (to_addr, subject, at) VALUES (?, ?, ?)`,
		c.Email, "A card for you: "+title, c.At.Unix()); err != nil {
		return err
	}
	tx.Touch(Topic(board), CardTopic(c.Card))
	if c.Wake != nil {
		tx.AfterCommit(c.Wake) // on the writer's goroutine, after the commit: must not block
	}
```

```go source=site/internal/docs/example/board/outbox.go
// Wake asks the sender to look at the outbox now. It never blocks, so a
// command can pass it to tx.AfterCommit.
func (o *Outbox) Wake() {
	select {
	case o.wake <- struct{}{}:
	default: // a pass is due already
	}
}
```

The sender itself is a background job: [Migrations, jobs and imports](/docs/guides/migrations-and-jobs#background-jobs) has it.

## Statements

The writer prepares each SQL text once, on its connection, and keeps the 512 used last. modernc's SQLite parses and plans every statement that has no prepared one, and in kanban that took 41% of the writer's CPU before the cache. So build SQL from constants and pass values as arguments.

SQL built at runtime, such as an `IN` list with a mark per value, would take a place in the cache for every length of the list. It runs through `tx.ExecUncached`, `tx.QueryUncached` or `tx.QueryRowUncached`, which prepare it for one call:

```go source=site/internal/docs/example/board/commands.go
func (c ArchiveCards) Apply(tx *writer.Tx) error {
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(c.Cards)), ", ")
	args := []any{c.At.Unix(), c.Board}
	for _, id := range c.Cards {
		args = append(args, id)
	}
	_, err := tx.ExecUncached(`UPDATE cards SET archived = ?
		WHERE archived IS NULL AND lane_id IN (SELECT id FROM lanes WHERE board_id = ?)
		AND id IN (`+marks+`)`, args...)
	if err != nil {
		return err
	}
	tx.Touch(Topic(c.Board))
	return nil
}
```

A list passed as one JSON argument keeps a single cached statement for any length: `AND id IN (SELECT value FROM json_each(?))`.

A cached statement serves one query at a time. Scan a `QueryRow` before the same query runs again: until then its statement is busy. A `Query` whose rows are still open makes the same query run on a statement of its own, and rows still open when the command returns are closed then.

## An event-sourced command

The writer applies one command at a time, so an event-sourced command needs no optimistic retry loop either. One `Apply` loads the aggregate from its projection, decides, appends the events and updates the projection:

```go source=site/internal/docs/example/board/events.go
func (c RenameCard) Apply(tx *writer.Tx) error {
	var card Card
	var board int64
	err := tx.QueryRow(`SELECT c.title, l.board_id FROM cards c JOIN lanes l ON l.id = c.lane_id
		WHERE c.id = ?`, c.Card).Scan(&card.Title, &board)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	events, err := decideRename(card, c.Title)
	if err != nil || len(events) == 0 {
		return err
	}
	for _, e := range events {
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO card_events (card_id, type, data, at) VALUES (?, 'renamed', ?, ?)`,
			c.Card, string(data), c.At.Unix()); err != nil {
			return err
		}
		card.Title = e.To
	}
	if _, err := tx.Exec(`UPDATE cards SET title = ?, version = version + 1 WHERE id = ?`, card.Title, c.Card); err != nil {
		return err
	}
	tx.Touch(Topic(board), CardTopic(c.Card))
	return nil
}
```

`decideRename` is a pure function of the card and the new title, so a test calls it without a database. Views render the projection, the `cards` row. A render that replays the events grows with the history.

## What a command must not do

- Wait for the network, a lock or a channel: the whole queue waits with it. Use `AfterCommit` or a job.
- Call `Do`, `Flush` or `Exclusive` on its own writer: the writer is busy running the command, so the call never returns.
- Render HTML. Views render, in their own read transactions, once per tick.
- Run several autocommits for one change. One command, one `Apply`, one savepoint: a change made of an insert, an audit row and an outbox row commits or rolls back as one.

## Options

| `writer.Options` | Default | What it does |
| --- | --- | --- |
| `MaxBatch` | 512 | the most commands one transaction takes |
| `QueueSize` | 4096 | commands that may wait; `Send` returns `ErrQueueFull` beyond it, `Do` waits |
| `OnCommit` | none | gets the topics of each committed batch, sorted, without duplicates: `hub.Changed` |
| `OnSendError` | log | gets each failed command from `Send` |
| `OptimizeEvery` | 6 h | how often `PRAGMA optimize` runs between batches; it also runs on `Close` |
| `CheckpointPages` | 1000 | the WAL pages between the writer's checkpoints, with `sqlite.CheckpointWriter` |
| `Log` | `slog.Default()` | panics, failed batches, failed `Send`s, maintenance errors |

`w.Stats()` counts batches, commands, failures, the largest batch, the queue's length, the last commit's time and the p99 of the last 1024 commits. [Metrics and monitoring](/docs/guides/monitoring) puts them in Prometheus, and [Performance](/docs/performance/writer) has what the writer does on one machine.
