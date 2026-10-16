---
title: Event sourcing
nav: Events
headline: Event sourcing: streams, deciders and projections
summary: Streams of events in the app's SQLite file with lith/events. Commands decide from each stream's state, and projections commit with the events or are rebuilt from them.
description: Event sourcing with lith/events: streams in the app's SQLite file, deciders with a state row, projections in the same commit, rebuilds, catch-up.
order: 2
---

`lith/events` keeps an event log in the app's own database, beside its other tables. A command appends events to a stream inside its `Apply`, and the events, the stream's state and the inline projections commit in the command's savepoint, or not at all. The module adds nothing between a handler and the writer: it is a few functions that a command calls, three tables, and a `Runner` that rebuilds and catches up in the background.

The writer applies one command at a time. A stream's version cannot change between the read and the append, so there is no retry loop. Positions are handed out and committed in one order, so a reader that keeps the last position it saw misses nothing. And an inline projection commits with its events, so a page never sees an event without its effect.

The board example event-sources one thing, a card's title. A card is a row of `cards`, which `AddCard` makes and `MoveCard` moves; its stream holds its renames, and two projectors keep `cards.title` and a history table current. The code is in `site/internal/docs/example/board/events.go`.

## Streams and event types

A kind of stream has a key type, `int64` or `string`, and an interface that its events implement. Each event type is defined once, under the name the table stores, through a helper of one line that makes the compiler check that the type belongs to the stream:

```go source=site/internal/docs/example/board/events.go
// CardEvent is an event of a card's stream. A card itself is a row of
// cards, which AddCard makes; its stream holds its renames.
type CardEvent interface{ cardEvent() }

// Renamed is a card's rename.
type Renamed struct {
	Title string `json:"title"`
}

func (Renamed) cardEvent() {}

// Cards is the kind of the cards' streams: a stream per card, by its id.
var Cards = events.NewStream[int64, CardEvent]("card")

func defineCard[T CardEvent](name string) events.Type[int64, CardEvent, T] {
	return events.Define(Cards, name, func(t T) CardEvent { return t })
}

// CardRenamed is the type of Renamed, stored as card.renamed.
var CardRenamed = defineCard[Renamed]("card.renamed")
```

Go cannot constrain one type parameter by another, so `events.Define` takes the conversion to the stream's interface as a function: a type that does not implement `CardEvent` fails to compile in `defineCard`. A stream's id comes from the stream, `Cards.ID(7)`, and takes only the stream's key type. Events are stored as JSON; the kind and the type names are what the table holds, so they never change once events are stored.

## A decider

A decider is what a command needs to decide, folded from the stream's own events, never from a table shaped for pages: the functional decider pattern. `Decide` turns the command and the state into events, `Evolve` folds one event into the state. Both are pure:

```go source=site/internal/docs/example/board/events.go
// titled is what a rename decides on: the title of the card's last
// rename, folded from its stream.
type titled struct {
	Title string
}

// Rename is the decider of RenameCard.
var Rename = events.Decider[RenameCard, CardEvent, titled]{
	Decide: decideRename,
	Evolve: func(s titled, e CardEvent) titled {
		switch e := e.(type) {
		case Renamed:
			s.Title = e.Title
		}
		return s
	},
	Version: 1,
}

// decideRename is the decision alone: no database, so a test runs it
// with eventstest.
func decideRename(c RenameCard, s titled) ([]CardEvent, error) {
	title := strings.TrimSpace(c.Title)
	switch {
	case title == "" || utf8.RuneCountInString(title) > MaxTitle:
		return nil, &writer.Refusal{Msg: fmt.Sprintf("A title has 1 to %d characters.", MaxTitle)}
	case title == s.Title:
		return nil, nil
	}
	return []CardEvent{Renamed{Title: title}}, nil
}
```

The store keeps each stream's state as JSON in a row of `lith_event_state`, written in the same savepoint as the events, so a command reads one row instead of replaying the stream. A decider is made for one command type; the commands of one stream kind share the state type, `Evolve` and `Version`, each with a `Decide` of its own, and `Execute` refuses a decider whose state or version differs from the first it saw for the kind.

`Version` is the version of `Evolve` and of the state's shape. Each row records the version it was folded with, and a stream whose row has another version is folded again from its events at its next command. Bump it whenever `Evolve` or the state change; a new state field that old events fill needs the bump too. Fields of the state must survive JSON: unexported fields are lost when the row is written.

## Running a command on a stream

`events.Execute` does the rest in the command's `Apply`: it reads the state row, checks the expected version, decides, appends the events, saves the row and runs the inline projectors:

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

The card's existence is a fact of `cards`, not of its stream, so the command checks it there. An error from any step fails the command, and its savepoint takes everything back: no event, no state row, no projection change, no topic. A refusal from `Decide` is an error like any other, here a `*writer.Refusal` that the board's `Act` shows in the tab.

The expected version is the third argument after the stream's id. A stream's version counts its events, and the version that matters is the one the page showed when the person acted:

| `events.Expected` | Appends when |
| --- | --- |
| `events.Any` | always |
| `events.NoStream` | the stream has no events yet |
| `events.Exactly(v)` | the stream is at version `v` |

The check comes before the decision. A stream at another version fails the command with an error that wraps `events.ErrConflict`, and nothing changes. The board's pages show a card's title but not its stream's version, so a rename takes the last word; a page that shows the version sends it with the action, and the command passes `events.Exactly(c.Seen)`.

`events.Meta` records who and what caused the event: the actor (a user, a session), a correlation id (the request) and a causation id (the command's op id, which `writer.Once` deduplicates). `At` defaults to the time of the append and is stored in Unix milliseconds, so events of the same second keep their order on a page; their order in the log comes from their positions.

`events.Append` appends events that need no decision, such as an import's, with the same version check and the same projectors. It drops the stream's state row, so the stream's next `Execute` refolds it from all its events. Go infers the stream's event type from its id and refuses a struct in its place, so a single event goes through `events.One[CardEvent](Renamed{...})`, and several through `[]CardEvent{...}...`.

## Testing a decider

`lith/events/eventstest` tests a decider as given, when, then: the events the stream holds, a command, and the events or the error it causes. It runs the fold and the decision without a database:

```go source=site/internal/docs/example/board/board_test.go
func TestDecideRename(t *testing.T) {
	named := eventstest.For(board.Rename).Given(board.Renamed{Title: "Draft"})
	named.When(board.RenameCard{Card: 1, Title: " Final "}).Then(t, board.Renamed{Title: "Final"})
	named.When(board.RenameCard{Card: 1, Title: "Draft"}).Then(t)
	named.When(board.RenameCard{Card: 1, Title: " "}).ThenError(t, &writer.Refusal{Msg: "A title has 1 to 200 characters."})
}
```

`Then(t)` with no events checks that the command changes nothing. `ThenError` matches with `errors.Is`, or by the message, as for a refusal. Each case also decides on the state as the store keeps it, read back from JSON, and fails when the two decisions differ, as they do when the state has a field that JSON drops.

## Inline projectors

A projector keeps a read model current from one kind of stream. Its inline handlers run in the transaction of the command that appends, after the events are stored: pages read the model in their next tick, and never see an event without its effect. Each handler is tied to one event type, so it gets its event's struct without a type switch:

```go source=site/internal/docs/example/board/events.go
// history keeps card_history, every title a card had, for its panel. The
// table is the projector's own, so a new version of it is rebuilt from
// the events while pages read the old table.
var history = events.NewProjector(Cards, "card_history", 1,
	events.Handle(CardRenamed, func(tx *writer.Tx, r events.Record[int64, Renamed]) error {
		_, err := tx.Exec(`INSERT INTO card_history (card_id, version, title, at) VALUES (?, ?, ?, ?)`,
			r.ID, r.Version, r.Data.Title, r.At.Unix())
		tx.Touch(CardTopic(r.ID))
		return err
	}),
).Tables("card_history")

// Events is the board's event store. The app runs an events.Runner on it,
// which rebuilds a projector whose version changed.
var Events = events.Must(events.New(titles, history))
```

A projector handles or ignores (`events.Ignore`) every event type of its stream: `events.New` refuses one that forgets a type, two projectors with one name, and two streams with one kind, and `events.Must` turns that into a panic as the program starts. A handler touches the topics of what it changed, as a command does; `events.Execute` touches `events.Topic` besides, for views over the whole log.

`Tables` names the tables the projector writes, and only it: the app's migrations create them (`003_card_history.sql` here), and a rebuild replaces them. The board's other projector, `card_titles`, updates `cards.title`, a table that moves change too, so it names no tables and a new version of it rebuilds nothing.

## The Runner

`events.Runner` keeps a store's projectors current in one database: it rebuilds what changed and runs the catch-up projectors. The app starts it beside the writer, on a context that ends before the writer closes:

```go source=site/internal/docs/example/board/board_test.go
runCtx, stop := context.WithCancel(ctx)
runner := events.NewRunner(board.Events, db, w, events.Options{})
done := make(chan error, 1)
go func() { done <- runner.Run(runCtx) }()
defer func() {
	stop() // before the writer closes
	<-done
}()
```

`Run` first records new projectors and changed versions in `lith_projections`, in one command, and returns an error there, such as a projector's missing table. In commands, a missing table stops only its own stream kind: `Execute` and `Append` on that kind fail with an error that names the table, and the other kinds go on. Without a runner, inline projectors still run in every command; nothing is rebuilt and no catch-up projector moves. `runner.Stats()` lists each projector with its version, its status (live, rebuilding or parked), its position and its last error.

## Rebuilds

A new version of an inline projector, or a projector new to a database whose stream has events, is rebuilt from the events:

1. The runner creates an empty copy of each of its tables, `card_history__v2` with its indexes, from the table's own `CREATE` statements.
2. It fills the copy in batches of `Options.Batch` events (500), each a command of its own, so other commands run between them. The handlers write their tables by name, so for the length of each batch the copy takes the table's name and the table steps aside. The batches run in `tx.Untouched`, which drops the topics the handlers touch: no page reads the copy.
3. The batch that finds fewer events left is the swap: it applies them, and the copy keeps the table's name. Pages read the old table until that batch commits and the new one from then on, never a table half built. The swap touches the projector's topic, `lith:projection:card_history`, for views that list it.
4. The next command drops the old table. That is one `DROP TABLE`, as long as SQLite takes to free the table's pages: for a large projection, one long command that the other commands wait for.

While it rebuilds, pages stay current: the events of new commands reach the old table too, through the projector's new handlers, each projector in a savepoint of its own (`tx.Savepoint`), and touch their topics. The new handlers may not fit the old table, a column the new version adds, say: a failure there is logged and undone, and the person's command goes on. The copy gets every event all the same, and the swap replaces the table. Commands decide from their streams' state rows, not from projections. A rebuild that stops, with a deploy, goes on from its last batch with the next runner.

On the development machine, a rebuild of 100,000 events took about a second in 200 commands. Every batch renames tables, a schema change, so each statement a reader has prepared is prepared again after each batch, 53 µs for a lookup by id, which did not show in the readers' mean lookup of 4.4 µs ([the numbers](https://github.com/zweiundeins/lith/blob/main/bench/RESULTS.md)).

The tables are renamed with `PRAGMA legacy_alter_table` on, so views and triggers elsewhere that name the table go on naming whatever table holds the name. What the module asks of a projector's tables: only the projector writes them, they have no triggers of their own, and no foreign key points at them. After a rebuild to version 2, an index `x` of the table is named `x__v2`, since SQLite cannot rename an index. A migration that changes such an index drops both names, `DROP INDEX IF EXISTS x` and `DROP INDEX IF EXISTS x__v2`, before it creates the new one.

## Catch-up projectors

A catch-up projector's handlers (`events.After`) run after the commit, outside the writer, from a checkpoint the projector keeps in `lith_projections`: for mail, a search service or another system's API. The writer wakes them when a commit appends their events; nothing polls. A projector handles an event at least once, so the event's position goes along as the idempotency key:

```go source=events/example_test.go
hook := events.NewProjector(Cards, "card_hook", 1,
	events.After(Created, func(ctx context.Context, r events.Record[int64, CardCreated]) error {
		body := strings.NewReader(fmt.Sprintf(`{"card": %d, "title": %q}`, r.ID, r.Data.Title))
		req, err := http.NewRequestWithContext(ctx, "POST", "https://hooks.example.com/cards", body)
		if err != nil {
			return err
		}
		req.Header.Set("Idempotency-Key", strconv.FormatInt(r.Position, 10))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return err // retried with backoff, then parked
		}
		res.Body.Close()
		if res.StatusCode >= 300 {
			return fmt.Errorf("the hook answered %s", res.Status)
		}
		return nil
	}),
	events.Ignore(Renamed),
	events.Ignore(Archived),
).SkipHistory() // a new hook posts the cards made from now on
```

A failed event is tried again after `Options.Backoff` (a second), then after twice that, up to `MaxBackoff` (a minute). After `Retries` failures (8, about two minutes) the projector parks: it stops at the event, `lith_projections` shows `parked: ` and the error, and so does `Stats`. One broken mail holds up only its own projector. `runner.Resume(name)` tries the event again, and so does the next `Run`. A new catch-up projector, and one whose version changed, starts at the first event, or after the last one with `SkipHistory`.

## Monitoring

`runner.Stats()` has the database's head, the last position committed, and each projector's lag: how many positions a catch-up projector or a rebuild is behind the last event of its stream kind. Every kind shares the positions, so at most that many events are left; a live inline projector commits with its events and has no lag. The runner reads the head as it starts and moves it after each commit that appends, so reading it costs no query. `store.Stats()` counts the events committed by stream kind, the appends that `ErrConflict` refused, and the refolds: streams whose state `Execute` folded again from their events, after a decider's `Version` changed or `Append` dropped the state row.

`lith/prom` exports them as `<namespace>_events_*` when the app passes the runner and the store, `prom.Sources{Writer: w, Hub: hub, Events: runner, Store: board.Events}`. The [lith apps dashboard](/docs/guides/monitoring#the-dashboard) shows them in its Events row, and [two alerts](/docs/guides/monitoring#alerts) fire for a projector parked for a minute and for a live catch-up projector more than 1,000 positions behind for 10 minutes.

## Reading events

`events.Load` yields a stream's events and `events.Since` every event after a position, as iterators over the transaction they are given: a GET's, a render's `sqlite.QuerierFrom(ctx)`, a command's. Breaking off the loop closes the rows. `Load` decodes and upcasts each event; `Since` gives them as stored, as `events.Raw`, for an activity feed whose view lists `events.Topic`, and `events.Decode` turns one into its type.

Pages render from projections. A render that replays a stream grows with its history: the [review checklist](/docs/review) flags it.

## Looking back to a position

A position names a moment of the whole log, so a page can show the app as it stood at any of them, the position of a slider, say. Three reads take an upper position:

- `events.LoadUntil(ctx, store, q, id, after, upto)` yields a stream's events stored up to position `upto`, and `events.SinceUntil(ctx, q, after, upto)` every event between two positions.
- `events.StateAt(ctx, store, q, decider, id, upto)` folds a stream's events up to `upto` into the decider's state: what a command on it decided from at the time, with the stream's version then.
- A projection as of a position is a table of its own whose rows carry the positions between which they held. A change ends the current row at the event's position and starts the next row there, and one query reads the rows that held at a position:

```go source=events/asof_test.go
var titleHistory = events.NewProjector(Cards, "title_history", 1,
	events.Handle(Created, func(tx *writer.Tx, r events.Record[int64, CardCreated]) error {
		_, err := tx.Exec(`INSERT INTO title_history (card_id, title, valid_from) VALUES (?, ?, ?)`, r.ID, r.Data.Title, r.Position)
		return err
	}),
	events.Handle(Renamed, func(tx *writer.Tx, r events.Record[int64, CardRenamed]) error {
		if err := endRow(tx, r.ID, r.Position); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO title_history (card_id, title, archived, valid_from)
			SELECT card_id, ?3, archived, ?1 FROM title_history WHERE card_id = ?2 AND valid_to = ?1`, r.Position, r.ID, r.Data.To)
		return err
	}),
	events.Handle(Archived, func(tx *writer.Tx, r events.Record[int64, CardArchived]) error {
		if err := endRow(tx, r.ID, r.Position); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO title_history (card_id, title, archived, valid_from)
			SELECT card_id, title, 1, ?1 FROM title_history WHERE card_id = ?2 AND valid_to = ?1`, r.Position, r.ID)
		return err
	}),
).Tables("title_history")

// endRow ends a card's current row at position pos.
func endRow(tx *writer.Tx, card, pos int64) error {
	_, err := tx.Exec(`UPDATE title_history SET valid_to = ? WHERE card_id = ? AND valid_to IS NULL`, pos, card)
	return err
}
```

```go source=events/asof_test.go
rows, err := q.QueryContext(ctx, `SELECT card_id, title FROM title_history
	WHERE valid_from <= ?1 AND (valid_to IS NULL OR valid_to > ?1)`, pos)
```

The table's key is (`card_id`, `valid_from`), so one card's row at a position is a lookup. The test beside it checks, at every position of a random history, that the table, a replay of the log and each card's `StateAt` agree. A rebuild fills the table like any other, so the history table can come long after the events.

What a look costs on the development machine, for 100 cards and a log of 10,000 events at a random position, in one read transaction: 0.46 ms from the history table, 12.6 ms for a replay of the log up to the position (`SinceUntil`, `Decode` and the fold, 2.5 µs an event), 0.12 ms for one card's `StateAt` over its events up to the position. A slider gives each tab a position of its own, so the view that shows the past is the tab's, rendered for that tab in every tick its slider moves: plan the history table for whole pages and `StateAt` for one stream's page, and key a shared view by the position when tabs can land on the same ones.

## Event shapes that change

A new optional field needs nothing: old events decode without it, at its zero value. A change beyond that gets a new type with a new name, such as `card.renamed.v2`, and an upcaster reads the old type as the new one: `events.Upcast(old, new, fn)`, passed to `events.New`, with `fn` turning the old struct into the new. Stored events are never changed. Projectors and deciders see only the newest type, and a command can no longer append the old one. Upcasters chain, from v1 to v2 to v3.

Each event type keeps a JSON sample in `testdata`, as it was first stored, and a test decodes and upcasts every sample. A field renamed instead of upcast then fails in CI, not on old rows in production:

```go source=site/internal/docs/example/board/board_test.go
func TestEventSamples(t *testing.T) {
	eventstest.Samples(t, board.Events, board.Cards, os.DirFS("testdata/card"))
}
```

`Samples` fails for a type without a sample, and for a sample with a field its type lacks.

## Retention

Event tables grow like any other, and each stream kind needs a rule for what it keeps:

- `events.Forget(store, tx, id)` deletes a stream's events and its state row: a person's erasure, or a card archived long ago. Nothing can be rebuilt from them afterwards, so the command deletes the projections' rows of the stream too.
- `events.Prune(store, tx, Cards, before, limit)` deletes a kind's events stored before a time, oldest first, for a sweep. It keeps the state rows and the projections, so commands and pages go on as before, but a rebuild or a new projector sees only the events left, and a decider whose version changes keeps the state of a stream that lost its first events as it was. It deletes nothing a catch-up projector of the kind, or a rebuild under way, has not passed.

A stream whose events are gone cannot be rebuilt. Keep personal data out of events where history must stay, or delete whole streams with their projections' rows. The board's sweep forgets the streams of cards archived a month ago, in the same command that deletes their history:

```go source=site/internal/docs/example/board/commands.go
func forgetCards(tx *writer.Tx, before int64) error {
	rows, err := tx.Query(`SELECT id FROM cards WHERE archived < ?`, before)
	if err != nil {
		return err
	}
	var gone []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		gone = append(gone, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, id := range gone {
		if _, err := events.Forget(Events, tx, Cards.ID(id)); err != nil {
			return err
		}
	}
	return nil
}
```

Positions use `AUTOINCREMENT`, so a position is never handed out twice, also after retention deleted the newest events: a catch-up projector that keeps its position never skips the event that would otherwise take a deleted one's place.

## The tables

The first command that needs them creates three tables, as `writer.Once` creates `lith_ops`:

| Table | Holds |
| --- | --- |
| `lith_events` | an event per row: its position, the stream's kind and key, its version, its type, its JSON, the time in Unix milliseconds, the actor, correlation and causation |
| `lith_event_state` | a row per stream: its version, the decider's version it was folded with, and the state as JSON |
| `lith_projections` | a row per projector: its version, its position (a catch-up projector's checkpoint, a rebuild's progress) and its status |

The tables are the app's to read: a query of its own over `lith_events`, such as the newest events first for a feed, goes through the read pool like any other.

## What an event costs

An `Execute` with one inline projector writes six B-trees: the event's row, its two indexes and `sqlite_sequence`, the state row and the projection. On the development machine, an Intel i5-13500, on 16 October 2026 with other jobs running, `BenchmarkExecute` appended 6,675 events a second for one producer and 18,342 for 200 with `synchronous=NORMAL`, where `BenchmarkWriter`'s transfers of four statements reached 24,404 and 81,087 in the same hour ([the numbers](https://github.com/zweiundeins/lith/blob/main/bench/RESULTS.md)).

