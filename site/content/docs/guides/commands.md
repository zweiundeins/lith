---
title: Commands and the writer
nav: Commands
headline: Commands: every change through the one writer
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

// Validate runs before the command is queued when it is handed to Send or
// Do itself: they return its error wrapped in writer.ErrInvalid. Inside
// an Act it runs in the writer instead, and the tab reads its message as
// a refusal: a *writer.Refusal, whose message is for the person.
func (c AddCard) Validate() error {
	if c.Lane <= 0 {
		return &writer.Refusal{Msg: "A card needs a lane."}
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(c.Title)); n == 0 || n > MaxTitle {
		return &writer.Refusal{Msg: fmt.Sprintf("A title has 1 to %d characters.", MaxTitle)}
	}
	return nil
}

func (c AddCard) Apply(tx *writer.Tx) error {
	_, err := c.Insert(tx)
	return err
}

// Insert adds the card and returns its id. A full lane refuses it, and so
// does one deleted since the page showed it.
func (c AddCard) Insert(tx *writer.Tx) (int64, error) {
	var board int64
	var limit int
	err := tx.QueryRow(`SELECT board_id, max_cards FROM lanes WHERE id = ?`, c.Lane).Scan(&board, &limit)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, &writer.Refusal{Msg: "This lane is gone."}
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

- `Validate` checks what the command can check alone, before it is queued. `Send` and `Do` call it, when the command has one, and return its error wrapped in `writer.ErrInvalid`. A page's action runs inside `Act`, which calls `Validate` itself and records its message as a refusal ([Refusals](#refusals)), so its messages are written for the person who typed the title.
- The check that reads before it writes, whether the lane has room, is inside `Apply`. The command sees every command queued before it, and nothing changes between its read and its write, so there is no retry loop.
- `tx.Touch(Topic(board))` names what changed. After the batch commits, the writer hands the touched topics to `Options.OnCommit`, which is `hub.Changed`, and every view that lists one of them renders again in the next tick. A command that touches nothing leaves every page as it was.

An error from `Apply`, or a panic, rolls back the command's savepoint: its changes, its topics, its `AfterCommit` functions and its published values go, and the other commands of the batch commit without it. A panic comes back as an error that wraps `writer.ErrPanic`, and the log gets its stack.

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

### Do or Send

Use `Do` for what a person asked for: a sign-up, a card moved, a message sent. Its 204 then means the change is stored. If the batch fails, the handler knows it before it answers, and tells the tab on its stream ([The handler's answer](#the-handlers-answer)): a page that waits for an op id would otherwise wait for nothing. The wait costs little: commands share a commit, so with `synchronous=NORMAL` a command commits in about half a millisecond at p50, and its frame follows in the next tick anyway. Handlers that wait for their commits also hold the load back by themselves, where a flood of `Send`s fills the queue and gets `ErrQueueFull`.

Use `Send` for updates the next one replaces, which nobody waits for: a pointer's position, a heartbeat, a "last seen". A lost one costs nothing, and waiting for each would only slow down a page that sends fifteen a second. The site's cursor party sends its moves this way.

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

A handler hands the command over and answers 204. What became of it reaches the page on the tab's stream, and the status code carries nothing a person reads. Datastar's essay [I'm a Teapot](https://data-star.dev/essays/im_a_teapot) says it in one line: "The 4xx and 5xx series are for machines and should never be seen by humans."

### Outcomes, notices and the page's own words

What a person must learn about an action falls into three cases, by who knows it:

1. **An outcome**: the person caused it. A validation error, a refusal, a quota used up, a card that is gone or a board the person lost access to meanwhile, a payment wall, a conflict with the tab's own earlier action, an upload that is too large or of the wrong type. The command records it in the tab's row (`Act` with a `*writer.Refusal`, [Refusals](#refusals)), the tab's view renders it, and the action answers 204. A check the handler makes before any command, the plan that the person's account holds, say, still records its refusal through an `Act`-style command: the payment wall reaches the tab's row like any refusal. An upload's checks are outcomes too, though it goes over plain `fetch` rather than a Datastar action ([Uploads](/docs/guides/pages#uploads)). An outcome survives a reconnect and a reload.
2. **A notice**: the person must know, but no command decided it, so no row can hold it. The writer failed, a rate limit sized for abuse turned the action away, the server is stopping. The handler sends it on the tab's open stream with `hub.Notify` and answers with the status alone; a server that stops sends `live.Options.Stopping` on every stream. A notice is best effort: a reconnect or a reload loses it.
3. **What only the browser knows**: the network is down, the stream does not connect, a request never came back. The page says so in its own words, from Datastar's `datastar-fetch` event or its watchdog, never from a response's body.

An outcome and a notice both carry the op id of the action they answer, so in the first two cases the stream ends the page's pending state. The page ends it itself only in the third: when a request never got through, and when its stream connects again and the tab's row lacks the op's ack, which covers a notice that a reconnect lost. The pending state ends on a signal, never on a clock ([Pages and Datastar](/docs/guides/pages#op-ids-and-the-pending-state)); a command that runs long records its acceptance first ([Long operations](/docs/guides/pages#long-operations)).

The status code stays for machines: whether to retry, a stream to end, a request that no page of ours sends. A body that does not parse, or an id that no page holds, gets a bare 400: it comes from a bug or an attack, and "If you get at client error or server error when you control both sides then it's a bug, and you should be fixing it."

### The board's handlers

`do` hands an action's command to the writer, and `Do`'s error says which answer to give:

```go source=site/internal/docs/example/web/server.go
func (s *Server) do(w http.ResponseWriter, r *http.Request, a action, cmd writer.Command) bool {
	switch err := s.w.Do(r.Context(), cmd); {
	case err == nil:
		return true
	case errors.Is(err, writer.ErrInvalid):
		w.WriteHeader(http.StatusBadRequest) // Act refuses what a person can get wrong: this is a bug
	case errors.Is(err, writer.ErrClosed):
		w.WriteHeader(http.StatusServiceUnavailable) // the hub stopped first: Options.Stopping told the tab
	default:
		s.failed(w, r, a, err)
	}
	return false
}
```

- `writer.ErrInvalid` never comes from what the person typed: `Act` runs the command's `Validate` and records a failure as a refusal. It comes from `Act`'s own check of the session, the tab and the op, which every page of ours passes.
- `writer.ErrClosed` comes once the writer has stopped taking commands. Run the writer on a context that outlives the HTTP server and call `w.Close()` after `srv.Shutdown`, so the actions that arrive while the server drains still commit ([Wiring](/docs/getting-started/wiring#starting-and-stopping) has the order). The hub stops before both, and every tab's stream has ended with the `Stopping` notice by then, so the 503 tells the tab nothing new.
- Any other error is the writer's: a batch that fails as a whole, at its `BEGIN`, a savepoint or its `COMMIT`, fails every command in it that had not failed on its own, with the batch's error. `failed` logs it and sends the tab a notice:

```go source=site/internal/docs/example/web/server.go
func (s *Server) failed(w http.ResponseWriter, r *http.Request, a action, err error) {
	s.log.Error("action failed", "path", r.URL.Path, "err", err)
	s.notify(r.Context(), a, "That could not be saved. Try again in a moment.")
	w.WriteHeader(http.StatusInternalServerError)
}

// notify sends the tab of action a a notice in place of its outcome, with
// the op id that ends its pending state. The tab's view puts the outcome
// back with its next frame, and a stream that reconnects never gets it.
// Each op key has a notice key of its own, so a notice for one key never
// replaces another's that has not gone out; an action without an op has
// one key for all.
func (s *Server) notify(ctx context.Context, a action, msg string) {
	ev, err := notice(ctx, a.Key, a.Op, msg)
	if err != nil {
		s.log.Error("notice", "err", err)
		return
	}
	key := "notice"
	if a.Op != "" {
		key += ":" + a.Key
	}
	s.hub.Notify(streamID(edge.SessionID(ctx), a.Tab), key, ev)
}

// streamID names a tab's stream after its session too, so that an action
// reaches only the streams of its own session: whoever learns a tab id
// can neither watch views for it nor send it a notice.
func streamID(sid, tab string) string {
	return sid + "." + tab
}
```

The notice is a frame like the tab view's: the message in the outcome's place, and the op id as its key's ack in `$_ops`, refused, so that the page keeps what the person typed:

```go source=site/internal/docs/example/web/views.go
func notice(ctx context.Context, key, op, msg string) ([]byte, error) {
	html, err := patch(ctx, Notice(msg))
	if err != nil || op == "" {
		return html, err
	}
	ack, err := json.Marshal(map[string]any{"_ops": map[string]board.Ack{key: {Op: op, Refused: true}}})
	if err != nil {
		return nil, err
	}
	return slices.Concat(html, sse.PatchSignals(ack, false)), nil
}
```

`hub.Notify(streamID, key, frame)` sends the frame once, through the stream's `WriteTick` or `Write` like a view's frame, and forgets it. It waits for the renders pending for the stream's views when it is called, those of the commit just before it among them, and goes out in the same write as their frames, after them: the commit's frame never overwrites it. A render among them that fails holds it no longer: it goes out after that tick without the view's frame. With none pending, it goes with the stream's next write. Like a frame, it waits while the stream holds its frames back: up to 250 ms on a slow link, and up to `live.AckTimeout`, 10 s, while a paced stream's window is full. The stream keeps one notice per key: a later notice with the same key replaces one that has not gone out yet, in its place, and the last one wins. The board acks per op key (`$_ops[key]`, one op pending per key), so it keys a notice by the op key. A page that acks per op (`$_outcomes[op]`) keys it by the op id instead: under one key, a later op's notice would drop an earlier one's, and that op would stay pending until the stream reconnects. The stream keeps 64 waiting at most (`live.MaxNotices`), dropping the oldest for a new key; an action without an op key, whose key the handler did not check, uses one fixed notice key. A notice for a stream that is not open does nothing, and `Notify` returns false. It goes nowhere else: no view keeps it, so the next frame of the tab's view puts the outcome back, and a stream that reconnects starts without it. The stream's ID names the session as well as the tab: with the bare tab id, any session that learned it could send the tab a notice with an ack of its choosing.

The board's `Options.Stopping` renders the same notice when the hub stops, before each tab's stream ends with `sse.Reconnect`. The new process's first frame of the tab's view replaces it:

```go source=site/internal/docs/example/web/views.go
func stopping(spec live.Spec) [][]byte {
	if spec.ID == "" {
		return nil
	}
	ev, err := notice(context.Background(), "", "", "The server is updating. Back in a moment.")
	if err != nil {
		return nil
	}
	return [][]byte{ev}
}
```

### Rate limits and quotas

Size a rate limit so that a person using the page never reaches it. The board allows 20 actions at once per address, then 5 a second, which nobody clicking gets near. An action over it gets 429, so that the page's retry waits, and its tab a notice:

```go source=site/internal/docs/example/web/server.go
if !s.limit.Allow(edge.ClientKey(r), 1, time.Now()) {
	s.notify(r.Context(), a, "Too many actions at once. Wait a moment, then try again.")
	w.WriteHeader(http.StatusTooManyRequests)
	return a, false
}
```

The check needs only the tab and the op, and the page sends them in the `Tab-Id`, `Op-Key` and `Op-Id` headers ([Op ids and the pending state](/docs/guides/pages#op-ids-and-the-pending-state)), so `decode` turns a request away, and sends its notice, before it reads or decodes the body.

A limit that a person can reach by using the app as intended is a rule of the product, not a rate limit. Model it as a command that counts in a table and refuses, a quota: "You have used this month's 20 scans." The person then reads it next to where they acted, and it survives a reload.

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

A card or a lane that is gone is a refusal too, "This card is gone.", not a 404: the page showed it, and someone archived it meanwhile, or the person lost access to its board. The id came from a page we rendered, so the person caused nothing wrong and reads why nothing changed.

A page shows a refusal next to where the user acted, and it learns of it the way it learns of everything else: from its stream. So the board's actions run inside `Act`, which records the outcome in the tab's row. `tx.Savepoint` runs the action's command in a nested savepoint: a refusal undoes the command's changes and drops its topics, and `Act` writes the tab's row all the same.

```go source=site/internal/docs/example/board/commands.go
func (a Act) Apply(tx *writer.Tx) error {
	state, err := Tab(tx.Context(), tx, a.Session, a.Tab)
	if err != nil {
		return err
	}
	state.Refusal = ""
	var refusal *writer.Refusal
	switch err := tx.Savepoint(a.run); {
	case errors.As(err, &refusal):
		state.Refusal = refusal.Msg
	case err != nil:
		return err
	}
	state.record(a.Key, a.Op, refusal != nil)
	return saveTab(tx, a.Session, a.Tab, state, a.At)
}
```

`record` keeps the op under the action's key, the add form's or a card's, with whether it was refused ([Several pending ops](/docs/guides/pages#several-pending-ops-on-one-page)).

`Act` runs the command's `Validate` itself, inside the savepoint, and records a failure as a refusal: a title that is too long is the person's input, and they read why next to the form. `Act`'s own `Validate` checks only the session, the tab, the key and the op, which the handler sets.

Every check a person can fail ends as a refusal in the tab's row: in `Apply`, or in a `Validate` that runs inside `Act`. A command handed to `Do` alone answers a failed `Validate` with `writer.ErrInvalid`, and the handler with a bare 400, which carries no notice: the page then ends the op in its own vague words, "That did not work", and the person never learns the reason ([Pages and Datastar](/docs/guides/pages#op-ids-and-the-pending-state)). Keep such checks for what no page of ours sends.

```go source=site/internal/docs/example/board/commands.go
func (a Act) run(tx *writer.Tx) error {
	if v, ok := a.Cmd.(writer.Validator); ok {
		if err := v.Validate(); err != nil {
			return &writer.Refusal{Msg: err.Error()}
		}
	}
	return a.Cmd.Apply(tx)
}
```

The tab's view renders the row, and [Pages and Datastar](/docs/guides/pages#op-ids-and-the-pending-state) shows how the page waits for it.

## Op ids and retries

A network repeats requests: a proxy retries, a user clicks twice, Datastar retries an action whose answer got lost. Each action carries an op id that the page made, and the handler wraps its command in `writer.Once` with a key of the session and the op:

```go source=site/internal/docs/example/web/server.go
act := board.Act{Session: sid, Tab: a.Tab, Key: a.Key, Op: a.Op, Cmd: cmd, At: time.Now()}
if s.do(w, r, a, writer.Once(sid+":"+a.Op, act)) {
```

When the command succeeds, `Once` records the key in the table `lith_ops`, in the same savepoint. A command whose key is recorded succeeds without running again. A failed command recorded nothing, so a retry runs it again and gets its error afresh. `Act` turns a refusal into a success that wrote the tab's row, so a retry of a refused action replays nothing either.

`writer.OnceResult` keeps an answer as well, for an action whose response carries one. Creating a board answers with the new board's address, and a retry must get the same address, not a second board:

```go source=site/internal/docs/example/web/server.go
op := writer.OnceResult(sid+":"+a.Op, board.CreateBoard{Name: a.Name})
if !s.do(w, r, a, board.Act{Session: sid, Tab: a.Tab, Key: a.Key, Op: a.Op, Cmd: op, At: time.Now()}) {
	return
}
if op.Result() == nil {
	w.WriteHeader(http.StatusNoContent) // refused: the tab's view says why
	return
}
```

The command is a `writer.ResultCommand`: its `ApplyResult` returns a few bytes, here the id. `OnceResult` records them in `lith_results` with the key, and also records a refusal with its message: a retry of a refused command gets the same `*writer.Refusal` without running. After `Do`, `op.Result()` holds the bytes, from this run or the recorded one, and `op.Replayed()` says which. Other errors record nothing.

The op runs inside `Act` like every action, so a name that fails `CreateBoard`'s `Validate`, and a refusal, reach the tab's row and its view; the answer is then 204 without a body. Only a board that was made sends the page there, in the answer ([Pages and Datastar](/docs/guides/pages#what-an-actions-answer-carries)).

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

## In-process listeners

`tx.Publish(v)` hands a value to the writer's subscribers once the batch has committed. Like an `AfterCommit` function, the value is dropped if the command fails, or the savepoint it was published in. `w.Subscribe[T](ctx, n)` returns a channel of the published values whose dynamic type is `T`, or implements `T` when `T` is an interface: `Subscribe[any]` gets all of them. A value published as `*Card` reaches no `Subscribe[Card]`. The channel closes when `ctx` ends or the writer stops. Every subscriber gets the same value, so publish values that nobody changes afterwards: a pointer, a map or a slice is shared.

The values arrive in commit order: batch by batch, the commands of a batch in their order, a command's values in the order it published them. The writer hands them over on its goroutine after the batch's `AfterCommit` functions and `OnCommit`, also when one of those panicked, and before any `Do` of the batch returns: by then the values are in the subscribers' channels, not yet handled. The writer never waits: each subscriber has a queue of `n`, and a value that finds it full is dropped for that subscriber alone. `w.Stats().Dropped` counts the drops and `Subscribers` the subscriptions open, and Prometheus gets them as `<ns>_writer_publish_dropped_total` and `<ns>_writer_subscribers`.

Publishing is best effort. A value is lost for a subscriber whose queue is full, and for all of them when the process stops between the commit and the delivery. A subscription gets the values the writer delivers while it is open, also one whose `tx.Publish` ran before `Subscribe` was called, and none delivered before; nothing is replayed. A listener that must not miss anything corrects itself from the data now and then, or reads [`lith/events`](/docs/guides/events) instead. `w.Exclusive` runs no command, so it publishes nothing: to tell the listeners, follow it with a `Do` whose command only publishes.

Pages need none of this. Views render from the database, and `tx.Touch` already renders every view that shows what changed. A passing effect, such as a pulse on a card someone just edited, comes from the data too: the row keeps when and by whom it was edited, and the view adds a class while the edit is recent. An outcome a tab must show, such as a saved form, a validation error or a refused move, is the tab's state, written by the command and rendered by the tab's view, so it survives a stream that reconnects and a reload.

`Publish` is for listeners in the process outside the views: notifications and mail, achievements, an admin's firehose, metrics, caches, webhooks. Which tool a change needs depends on who must hear of it, and on what may miss it:

| Tool | For | What can miss it |
| --- | --- | --- |
| `tx.Touch(topics...)` | the views that read the topics, which render again | nothing: views render from the database, so a page that reconnects or reloads gets the current state |
| `tx.AfterCommit(fn)` | a side effect of this one command, such as waking the outbox's sender | a crash between the commit and `fn`: keep the work in a table, as the outbox does, and let `fn` only wake it |
| `tx.Publish(v)` | in-process listeners outside the views | a full queue, a crash before the delivery, a listener that subscribes later: a listener that must be right reconciles from the data |
| [`lith/events`](/docs/guides/events) | projectors and other readers of a durable log | nothing: the events are rows of the command's transaction, and a projector catches up from its checkpoint after a restart |

The board publishes an `Assigned` value when a card gets someone:

```go source=site/internal/docs/example/board/commands.go
tx.Publish(Assigned{Card: c.Card, From: from, To: c.Email}) // for in-process listeners, such as Workload
```

`Workload` keeps each person's open cards in memory for a metric. It counts them from the cards table now and then, and the count must not race the values: a count read beside the writer could already hold an assignment whose value is still on its way, and the listener would apply it twice. So the count is a command too. It reads in the writer's transaction and publishes what it read, and the count reaches the listener in commit order with the `Assigned` values: it holds every assignment before it and none after. The count runs inside a writer batch, so every other write waits while it reads: keep such a count cheap, with an index that covers it, or infrequent.

```go source=site/internal/docs/example/board/workload.go
func (CountOpen) Apply(tx *writer.Tx) error {
	open, err := OpenCards(tx.Context(), tx)
	if err != nil {
		return err
	}
	tx.Publish(OpenCount(open))
	return nil
}
```

The listener subscribes to an interface that both values implement, asks for a count at once and every interval, replaces its state with a count and adds the values that follow it:

```go source=site/internal/docs/example/board/workload.go
func (l *Workload) apply(c WorkloadChange) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch c := c.(type) {
	case OpenCount:
		l.open = maps.Clone(c) // the count is shared, and this copy changes
	case Assigned:
		if l.open == nil {
			return // the first count, still to come, holds it
		}
		l.open[c.From]--
		l.open[c.To]++
		delete(l.open, "")
	}
}
```

Between two counts `Workload` is exact unless its queue dropped a value, or a change published none, such as an archive; the next count puts both right.

A command that publishes nothing costs the writer nothing more, and each subscriber costs it a type check per published value.

## Statements

The writer prepares each SQL text once, on its connection, and keeps the 512 used last. modernc's SQLite parses and plans every statement that has no prepared one, and in kanban that took 41% of the writer's CPU before the cache. So build SQL from constants and pass values as arguments.

SQL built at runtime, such as an `IN` list with a mark per value, would take a place in the cache for every length of the list. It runs through `tx.ExecUncached`, `tx.QueryUncached` or `tx.QueryRowUncached`, which prepare it for one call. Query code that takes a `sqlite.Querier` passes a ctx from `sqlite.Uncached(ctx)` instead: the `*writer.Tx` then runs the uncached variant, and a read connection or a render worker skips its cache the same way ([Reads](/docs/guides/reads#sql-built-at-runtime)).

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

The writer applies one command at a time, so an event-sourced command needs no optimistic retry loop either, and a stream's version cannot change between the read and the append. [`lith/events`](/docs/guides/events) does the work in the command's `Apply`: `events.Execute` reads the card's state row, decides, appends the event, saves the row and runs the inline projectors, all in the command's savepoint:

```go source=site/internal/docs/example/board/events.go
func (c RenameCard) Apply(tx *writer.Tx) error {
	var one int
	err := tx.QueryRow(`SELECT 1 FROM cards WHERE id = ?`, c.Card).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return events.Execute(Events, tx, Rename, Cards.ID(c.Card), events.Any, c, events.Meta{At: c.At})
}
```

The decision is a pure function of the command and the card's state, folded from its own events, so a test runs it without a database. Views render the projections, the `cards` row and `card_history`: a render that replays the events grows with the history. [Event sourcing](/docs/guides/events) has the stream, the decider and the projectors behind this command.

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

`w.Stats()` counts batches, commands, failures, the largest batch, the queue's length, the last commit's time, the p99 of the last 1024 commits, and the values published and dropped. [Metrics and monitoring](/docs/guides/monitoring) puts them in Prometheus, and [Performance](/docs/performance/writer) has what the writer does on one machine.
