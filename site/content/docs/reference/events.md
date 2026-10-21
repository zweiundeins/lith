---
title: events
headline: lith/events: event streams, deciders and projections
summary: Keeps an event log in the app's SQLite database: typed streams of events, deciders that decide from each stream's own state, and projectors that keep read models current.
description: Reference for lith/events: typed event streams in the app's SQLite file, deciders that keep a state row, inline and catch-up projectors, rebuilds.
order: 3
---

Import path `github.com/zweiundeins/lith/events`. Generated from the doc comments in [events/](https://github.com/zweiundeins/lith/tree/main/events).

It adds nothing between a handler and the writer. A command calls Execute or Append in its Apply, and the events, the stream's state row and the inline projections commit in the command's savepoint, or not at all. The writer applies one command at a time, so a stream's version cannot race, positions commit in the order they are handed out, and an inline projection is never behind its events.

#### Streams and event types {#hdr-Streams_and_event_types}

A kind of stream has a key type, int64 or string, and an interface that its event types implement. Each event type is defined once, under the name the table stores, through a helper of one line that makes the compiler check that the type belongs to the stream:

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

func defineCard[T CardEvent](name string) events.Type[int64, CardEvent, T] {
	return events.Define(Cards, name, func(t T) CardEvent { return t })
}

var (
	Created = defineCard[CardCreated]("card.created")
	Renamed = defineCard[CardRenamed]("card.renamed")
)
```

Events are stored as JSON. A new optional field needs nothing: old events decode without it. A change beyond that gets a new type, and an Upcaster reads the old one as the new.

#### Deciders and Execute {#hdr-Deciders_and_Execute}

A Decider is a stream kind's write side for a command type: Decide turns the command and the stream's state into events, Evolve folds an event into the state. The commands of one kind share the state, Evolve and Version, each with a Decide of its own. The store keeps each stream's state as JSON in a row, written in the same savepoint as the events, so a command reads one row instead of replaying the stream:

```go
type card struct {
	Exists bool
	Title  string
}

var rename = events.Decider[Rename, CardEvent, card]{Decide: decideRename, Evolve: evolveCard, Version: 1}

func (c Rename) Apply(tx *writer.Tx) error {
	return events.Execute(store, tx, rename, Cards.ID(c.Card), events.Exactly(c.Seen), c,
		events.Meta{Actor: c.Session, Causation: c.Op})
}
```

The expected version is the one the page showed when the person acted (Exactly), or Any, or NoStream for a stream that must not exist yet. A stream at another version fails the command with ErrConflict, and nothing changes.

#### Projectors {#hdr-Projectors}

A Projector keeps a read model current from one kind of stream. An inline projector's handlers (Handle) run in the command's transaction: pages read its tables in their next tick and never see an event without its effect. A catch-up projector's handlers (After) run after the commit, outside the writer, for mail, a search service or another system's API. A projector handles or ignores every event type of its stream, or New refuses it.

```go
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
).Tables("card_titles")

var store = events.Must(events.New(cardTitles))
```

A Runner keeps the projectors current in a database. When an inline projector's version changes, it rebuilds the projector's tables: it fills a copy of each from every event, a command per batch, while pages read the old tables, which new events still reach, and swaps the copies in with the last batch. It runs the catch-up projectors from their checkpoints, woken by the commits that append their events; an event that keeps failing parks its projector, with the error in Stats.

Runner.Stats also has the last position committed and how far each catch-up projector or rebuild is behind its stream kind, and Store.Stats counts the events appended, the conflicts and the refolds; lith/prom exports both.

#### Reads {#hdr-Reads}

Load reads a stream's events and Since every event after a position, both as iterators over the transaction they are given. Pages render from projections: a stream's history only grows. LoadUntil, SinceUntil and StateAt read as of a position, for a page that shows the past, and Bounds has the first and the last position of a kind, the ends of a slider. Before the first command made the tables, the reads find no events and no error.

#### Retention {#hdr-Retention}

Forget deletes a stream's events, and Prune the events of a kind that a Retention lets go: those stored before a time, those beyond the newest N, or those both rules let go. A projection keeps what it built from them, but a rebuild after Prune sees only the events that are left. Compact closes the books instead: each stream that loses events keeps its last one, rewritten in place as a snapshot of its state, an event type the app defines on the stream like any other. A rebuild then makes what it made before, and as-of reads stay right from the newest snapshot on. That rewrite is the one place where a stored event changes. Keep personal data out of events, or forget the whole stream with its rows in the projections.

#### Tables {#hdr-Tables}

The first command that needs them creates four tables: lith\_events (the position, from AUTOINCREMENT so that it is never reused, the stream's kind and key, its version, the type, the JSON data, the time in Unix milliseconds, the actor, correlation and causation), lith\_event\_state (each stream's folded state), lith\_projections (each projector's version, position and status) and lith\_event\_kinds (the position up to which Compact rewrote a kind). Positions commit in order, so a reader that keeps the last position it saw misses no event.

## Constants

```go
const Topic = "lith:events"
```

Topic is touched by every command that appends or deletes events: a view over Since, such as an activity feed, lists it.

## Variables

```go
var (
	// Any appends at whatever version the stream is.
	Any = Expected{}
	// NoStream appends only to a stream without events.
	NoStream = Expected{/* contains filtered or unexported fields */}
)
```

```go
var ErrConflict = errors.New("events: the stream is not at the expected version")
```

ErrConflict is the error, wrapped, of an append to a stream that is not at the expected version. Nothing was appended.

## Functions

### func Append {#Append}

```go
func Append[K Key, E any](st *Store, tx *writer.Tx, id StreamID[K, E], want Expected, meta Meta, evs ...E) error
```

Append appends events that need no decision, such as an import's or a migration's, after checking the expected version, and runs the inline projectors, in the command's transaction. The stream's state row, if a decider made one, is dropped, so the stream's next Execute refolds it from all its events.

### func Bounds {#Bounds}

```go
func Bounds[K Key, E any](ctx context.Context, q sqlite.Querier, s *Stream[K, E]) (first, last int64, err error)
```

Bounds returns the positions of the first and the last event of stream kind s, 0 and 0 without any: the ends of a slider over its history. After Compact, first is the newest snapshot, the oldest position at which every stream of the kind reads as it stood; the snapshots before it are of streams that did not change between them and it. It reads through q, with a seek in the log's index for each end.

### func CheckSample {#CheckSample}

```go
func CheckSample[K Key, E any](st *Store, s *Stream[K, E], name string, data []byte) error
```

CheckSample decodes a JSON sample of an event as Decode does, but refuses a field that the type of name lacks: a test that keeps a sample of each type, as it was stored, fails when a field is renamed instead of upcast.

### func Compact {#Compact}

```go
func Compact[K Key, E, T, C, S any](st *Store, tx *writer.Tx, snapshot Type[K, E, T], keep Retention, d Decider[C, E, S], of func(S) T) (int64, error)
```

Compact is Prune that closes the books: each stream that loses events keeps one, its last event that keep lets go, rewritten in place as a snapshot of the stream's state as of that event. The snapshot is an event of type snapshot, which the app defines on the stream like any other; of makes it from the state that d folds, as StateAt does. It keeps the event's position, version, time and metadata, and only its type and data change. d's Evolve sets the state from a snapshot, and every projector of the stream handles or ignores it, as New checks: a rebuild or a new projector makes from it what the events it replaces made, and a decider whose Version changes folds from it.

Rewriting a stored event is the one deliberate exception to events never changing: the snapshot holds what the events it replaces held, at the place of the last of them. Positions stay in order, so StateAt, LoadUntil and a projection that keeps positions read as before from Bounds' first position on, the newest snapshot. Each stream keeps one snapshot at most, so the snapshots are bounded by the number of streams; a later call replaces the snapshot of a stream that loses events again, and leaves the others alone.

Compact returns how many events it deleted. Like Prune, it keeps the state rows and the projections, and goes no further than a catch-up projector of the kind, or a rebuild under way, has passed. It refuses a stream whose first events Prune took, as it cannot fold its state. Its time grows with the events it takes, not with those the streams keep; and while Since only moves on, a writer's later calls do not read again the events a Limit left: a sweep with a Limit stays short on a long stream and a long backlog.

### func Decode {#Decode}

```go
func Decode[K Key, E any](st *Store, s *Stream[K, E], name string, data []byte) (E, error)
```

Decode decodes data as an event of type name of stream s, upcast to the newest type, as Load does: for the events Since reads, say.

### func Execute {#Execute}

```go
func Execute[K Key, C, E, S any](st *Store, tx *writer.Tx, d Decider[C, E, S], id StreamID[K, E], want Expected, cmd C, meta Meta) error
```

Execute runs a command on a stream, in the command's transaction: it reads the stream's state row (folding the stream's events when there is none, or one of another Version), checks the expected version, decides, appends the events, saves the state row and runs the inline projectors. An error from any of them leaves the command to fail, and its savepoint takes back everything. The deciders of a stream kind, one per command type, share the state type, Evolve and Version; Execute refuses a decider whose state type or Version differs from the first it saw for the kind.

### func Forget {#Forget}

```go
func Forget[K Key, E any](st *Store, tx *writer.Tx, id StreamID[K, E]) (int64, error)
```

Forget deletes every event of a stream and its state row, as a person's erasure asks. Nothing can be rebuilt from them afterwards: delete what the projections hold of the stream in the same command. A catch-up projector that has not reached them never sees them. It returns how many events it deleted.

### func Load {#Load}

```go
func Load[K Key, E any](ctx context.Context, st *Store, q sqlite.Querier, id StreamID[K, E], after int64) iter.Seq2[Record[K, E], error]
```

Load yields the events of a stream after version after (0 for all), in order, decoded and upcast, read through q: a GET's transaction, a live Render's sqlite.QuerierFrom(ctx), a command's \*writer.Tx. Breaking off the loop closes the rows. A render reads a projection rather than replaying a stream, whose history only grows; Load is for a stream's own history page, a test, a tool. After Compact, a stream that lost events starts at its snapshot. A database whose tables the first command has not made yet has no events.

### func LoadUntil {#LoadUntil}

```go
func LoadUntil[K Key, E any](ctx context.Context, st *Store, q sqlite.Querier, id StreamID[K, E], after, upto int64) iter.Seq2[Record[K, E], error]
```

LoadUntil is Load up to a position: the events of a stream after version after that were stored at or before position upto, the stream as it stood then. It reads no further than upto.

### func One {#One}

```go
func One[E any](e E) E
```

One returns e as the stream's event type E, for Append with a single event of a concrete type: Go infers E from the stream's id and then refuses a struct where it expects E.

```go
events.Append(st, tx, Cards.ID(7), events.Any, meta, events.One[CardEvent](CardRenamed{To: "Ship it"}))
```

### func Prune {#Prune}

```go
func Prune[K Key, E any](st *Store, tx *writer.Tx, s *Stream[K, E], keep Retention) (int64, error)
```

Prune deletes the events of stream kind s that keep lets go, oldest first, and returns how many it deleted: retention for a kind whose history need not stay, sent from a sweep. It keeps the state rows and the projections built from the events, so commands and pages go on as before; but a rebuild or a new projector sees only the events kept, so a stream whose events all went comes back as if it had none, and a decider whose Version changes keeps the state of a stream that lost its first events as it was. Compact keeps what they need. Prune deletes no event that a catch-up projector of the kind, or a rebuild under way, has not passed.

### func Since {#Since}

```go
func Since(ctx context.Context, q sqlite.Querier, position int64) iter.Seq2[Raw, error]
```

Since yields every event stored after position (0 for all), of every stream, in the order they were stored, as the table holds them: an activity feed is a view over it that lists Topic. Positions only grow, and they commit in order, so a reader that keeps the last position it saw misses no event. Breaking off the loop closes the rows. A database whose tables the first command has not made yet has no events.

### func SinceUntil {#SinceUntil}

```go
func SinceUntil(ctx context.Context, q sqlite.Querier, after, upto int64) iter.Seq2[Raw, error]
```

SinceUntil is Since up to a position: every event stored after position after and at or before position upto.

### func StateAt {#StateAt}

```go
func StateAt[K Key, C, E, S any](ctx context.Context, st *Store, q sqlite.Querier, d Decider[C, E, S], id StreamID[K, E], upto int64) (S, int64, error)
```

StateAt folds a stream's events up to position upto into d's state: what a command on the stream decided from then, for a page that shows the past. It returns the stream's version then too. It reads through q and stores nothing, and it folds from the stream's first event, or its snapshot after Compact, so it costs what the stream's history up to upto costs to read. It fails where retention left no state to fold: a stream whose first events Prune took, or a position before the stream's snapshot.

## Types

### type Decider {#Decider}

```go
type Decider[C, E, S any] struct {
	// Initial is the state of a stream without events.
	Initial S
	// Decide returns the events that cmd causes in state s, or the error
	// that refuses it. No events: nothing changes.
	Decide func(cmd C, s S) ([]E, error)
	// Evolve returns the state after event e.
	Evolve func(s S, e E) S
	// Version is the version of Evolve and of the state's shape. The store
	// keeps each stream's state as JSON in a row, with the version it was
	// folded with; a stream whose row has another version is folded again
	// from its events at its next command. Bump it whenever Evolve or the
	// state change.
	Version int
}
```

Decider is the write side of a kind of stream for commands of type C, in the functional decider pattern: what a command needs to decide, folded from the stream's own events. Decide and Evolve are pure, so a test runs them without a database (eventstest).

### type Expected {#Expected}

```go
type Expected struct {
	// contains filtered or unexported fields
}
```

Expected is the version a stream must be at for an append to go ahead: Any, NoStream or Exactly. A stream's version is the number of events appended to it.

#### func Exactly {#Exactly}

```go
func Exactly(v int64) Expected
```

Exactly appends only while the stream is at version v: the version the page showed when the person acted. Exactly(0) is NoStream.

### type Handler {#Handler}

```go
type Handler[K Key, E any] struct {
	// contains filtered or unexported fields
}
```

Handler is what a projector does with one event type.

#### func After {#After}

```go
func After[K Key, E, T any](t Type[K, E, T], fn func(ctx context.Context, r Record[K, T]) error) Handler[K, E]
```

After has a catch-up projector run fn for every event of type t, after the commit that stored it, outside the writer: for mail, a search service, another system's API. It may see an event more than once, after a failure or a restart; r.Position is the idempotency key to hand on. An error is retried with backoff; one that persists parks the projector (Runner).

##### Example {#example-After}

A catch-up projector posts every new card to another service after the commit. The service can see a card twice, after a failure or a restart; the event's position, sent as the idempotency key, tells it so.

```go
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
store := events.Must(events.New(hook))
_ = store // events.NewRunner(store, db, w, events.Options{}).Run(ctx) runs it
```

#### func Handle {#Handle}

```go
func Handle[K Key, E, T any](t Type[K, E, T], fn func(*writer.Tx, Record[K, T]) error) Handler[K, E]
```

Handle has an inline projector run fn for every event of type t, in the transaction of the command that appends it. fn writes the projector's tables and touches the topics of what it changed.

#### func Ignore {#Ignore}

```go
func Ignore[K Key, E, T any](t Type[K, E, T]) Handler[K, E]
```

Ignore says that a projector does not need events of type t, so that New can tell a forgotten type from one left out on purpose.

### type Key {#Key}

```go
type Key interface{ ~int64 | ~string }
```

Key is the type of a stream's ids: an integer or a text, which the tables store and index as they are.

### type Meta {#Meta}

```go
type Meta struct {
	// At is when the event was appended, stored in Unix milliseconds.
	// Append and Execute take the time then when it is zero; an import
	// sets it to when the event happened.
	At time.Time
	// Actor is who caused the event: a user, a session.
	Actor string
	// Correlation names what the event belongs to: a request, a job.
	Correlation string
	// Causation names what caused it: the command's op id, or the
	// position of the event a catch-up projector reacted to.
	Causation string
}
```

Meta is what an event records besides its data.

### type Options {#Options}

```go
type Options struct {
	// Batch is how many events one rebuild command applies, and how many
	// a catch-up projector reads at a time (default 500).
	Batch int
	// Retries is how many times a catch-up projector tries an event
	// before it parks (default 8).
	Retries int
	// Backoff is the wait after a catch-up projector's first failure on
	// an event, doubled after each further one up to MaxBackoff (defaults
	// 1s and 1 min): with the default Retries, an event that keeps failing
	// parks its projector after about two minutes.
	Backoff, MaxBackoff time.Duration
	// Log gets rebuilds, failures and parked projectors (default
	// slog.Default()).
	Log *slog.Logger
}
```

Options tune a Runner; zero fields take the defaults.

### type Part {#Part}

```go
type Part interface {
	// contains filtered or unexported methods
}
```

Part is what New takes: a \*Projector or an Upcaster.

### type Projector {#Projector}

```go
type Projector[K Key, E any] struct {
	// contains filtered or unexported fields
}
```

Projector keeps a read model current from the events of one kind of stream. Its handlers are inline (Handle), running in the transaction of the command that appends, or catch-up (After), running after the commit from a checkpoint; one projector has one or the other. Every event type of the stream is handled or ignored (Ignore), or New refuses the projector.

#### func NewProjector {#NewProjector}

```go
func NewProjector[K Key, E any](s *Stream[K, E], name string, version int, hs ...Handler[K, E]) *Projector[K, E]
```

NewProjector makes a projector of stream s's events. Its name is its row in lith\_projections, and version the version of its handlers: when an inline projector's version changes, Runner rebuilds its tables, and a catch-up projector starts again from the first event.

#### func (*Projector[K, E]) Name {#Projector.Name}

```go
func (p *Projector[K, E]) Name() string
```

Name returns the projector's name.

#### func (*Projector[K, E]) SkipHistory {#Projector.SkipHistory}

```go
func (p *Projector[K, E]) SkipHistory() *Projector[K, E]
```

SkipHistory starts a new catch-up projector, and one whose version changed, after the events stored by then, instead of at the first: for a mailer that must not send the mail of the past.

#### func (*Projector[K, E]) Tables {#Projector.Tables}

```go
func (p *Projector[K, E]) Tables(names ...string) *Projector[K, E]
```

Tables names the tables an inline projector writes, and only it: a rebuild fills a copy of each and puts the copies in their place. The app's migrations create and change them. After a rebuild to version N, an index x of a table is named x\_\_vN, as SQLite cannot rename an index: a migration that changes it drops both names (DROP INDEX IF EXISTS). The table a rebuild replaced is dropped in one command, which takes as long as SQLite needs to free its pages. A projector whose table is missing stops the commands of its stream kind until a migration creates it. Without tables, a version change rebuilds nothing.

#### func (*Projector[K, E]) Topic {#Projector.Topic}

```go
func (p *Projector[K, E]) Topic() string
```

Topic is touched when a rebuild puts the projector's new tables in place: a view that reads them lists it.

### type ProjectorStats {#ProjectorStats}

```go
type ProjectorStats struct {
	Name    string
	Version int
	CatchUp bool
	// Status is "live", "rebuilding" or "parked".
	Status string
	// Position is a catch-up projector's checkpoint, the last event it
	// handled, or how far a rebuild got.
	Position int64
	// Error is why a projector parked, or the failure of the event it
	// retries; it is cleared once the event goes through.
	Error string
	// Attempts counts the failures of the event a catch-up projector
	// retries.
	Attempts int
	// Lag is how far a catch-up projector or a rebuild is behind the last
	// event of its stream kind, in positions, which every kind shares: at
	// most that many events are left. An inline projector that is live
	// commits with its events and has none.
	Lag int64
}
```

ProjectorStats is one projector in a database.

### type Raw {#Raw}

```go
type Raw struct {
	Position int64
	Stream   string          // the stream's kind
	ID       any             // the stream's key: an int64 or a string
	Version  int64           // in its stream, from 1
	Type     string          // the name it was stored under, before upcasting
	Data     json.RawMessage // its JSON
	Meta
}
```

Raw is a stored event as the table holds it, for reads across streams.

### type Record {#Record}

```go
type Record[K Key, T any] struct {
	Position int64  // in the store, across streams; never reused
	ID       K      // the stream's key
	Version  int64  // in its stream, from 1
	Type     string // the name of its event type, after upcasting
	Data     T
	Meta
}
```

Record is a stored event of a stream with ids of type K, its data a T.

### type Retention {#Retention}

```go
type Retention struct {
	// Since keeps the events stored at or after this time. Zero sets no
	// rule of time.
	Since time.Time
	// Newest keeps this many of the kind's newest events. 0 sets no rule
	// of count.
	Newest int
	// Limit is the most events one call takes, the oldest first (0: no
	// limit), so that a sweep spreads a long backlog over its runs.
	Limit int
}
```

Retention says which events of a stream kind Prune and Compact keep. An event goes only when every rule that is set lets it go: with both, the events of the last 30 days stay, and the newest 1,000 also when they are older.

### type Runner {#Runner}

```go
type Runner struct {
	// contains filtered or unexported fields
}
```

Runner keeps a store's projectors current in one database: it rebuilds the inline projectors whose version changed, and runs the catch-up projectors after the commits that append their events. Without a running Runner, inline projectors still run in every command, but no rebuild happens and no catch-up projector moves.

#### func NewRunner {#NewRunner}

```go
func NewRunner(st *Store, db *sqlite.DB, w *writer.Writer, opts Options) *Runner
```

NewRunner makes a runner of st's projectors on db, whose writer is w.

#### func (*Runner) Resume {#Runner.Resume}

```go
func (r *Runner) Resume(name string) error
```

Resume has a parked catch-up projector try its event again.

#### func (*Runner) Run {#Runner.Run}

```go
func (r *Runner) Run(ctx context.Context) error
```

Run starts the runner's work and returns when ctx ends, once the work under way has stopped. At its start, in one command, it creates the tables, records new projectors and changed versions, and tries parked projectors again; an error there, such as a projector's missing table, is returned. Rebuilds then run one after the other, and each catch-up projector on its own goroutine. Cancel ctx before closing the writer.

#### func (*Runner) Stats {#Runner.Stats}

```go
func (r *Runner) Stats() Stats
```

Stats returns the runner's projectors as they stand.

### type Stats {#Stats}

```go
type Stats struct {
	// Head is the last position committed in the database, of any stream
	// kind: read as Run starts, then moved by each commit that appends.
	Head       int64
	Projectors []ProjectorStats // by name
}
```

Stats are a runner's projectors as they stand.

### type Store {#Store}

```go
type Store struct {
	// contains filtered or unexported fields
}
```

Store holds the projectors and upcasters of an app's event streams. It keeps no connection: Execute and Append run in a command's transaction, Load and Since in the transaction they are given. One Store serves every database the app opens, each with its writer, and keeps what it learns of a database on that writer (writer.Tx.Local).

#### func Must {#Must}

```go
func Must(st *Store, err error) *Store
```

Must returns st, and panics if err is not nil: for a store made at package level, events.Must(events.New(...)).

#### func New {#New}

```go
func New(parts ...Part) (*Store, error)
```

New makes a store of the projectors and upcasters. It refuses a projector that neither handles nor ignores an event type of its stream, two projectors with one name or one table, two streams with one kind, and an event type upcast twice. The first Execute, Append or Run checks once more, for event types defined after New ran.

#### func (*Store) Stats {#Store.Stats}

```go
func (st *Store) Stats() StoreStats
```

Stats returns the store's counters.

### type StoreStats {#StoreStats}

```go
type StoreStats struct {
	// Appended counts the events committed, by stream kind. The kinds of
	// the store's projectors and upcasters are there from the start.
	Appended map[string]int64
	// Conflicts counts the appends that ErrConflict refused.
	Conflicts int64
	// Refolds counts the streams whose state Execute folded again from
	// their events, as their state row was missing or folded by another
	// Version of the decider.
	Refolds int64
}
```

StoreStats are a store's counters since the process started, over every database the store serves.

### type Stream {#Stream}

```go
type Stream[K Key, E any] struct {
	// contains filtered or unexported fields
}
```

Stream is a kind of stream, such as "card": the type of its ids and of its events, and the event types defined on it. Make one per kind at package level with NewStream, and define its event types with Define.

#### func NewStream {#NewStream}

```go
func NewStream[K Key, E any](kind string) *Stream[K, E]
```

NewStream makes a kind of stream. The kind is what the tables store in their stream column, so it must not change once events are stored.

#### func (*Stream[K, E]) ID {#Stream.ID}

```go
func (s *Stream[K, E]) ID(key K) StreamID[K, E]
```

ID names one stream of the kind.

#### func (*Stream[K, E]) Kind {#Stream.Kind}

```go
func (s *Stream[K, E]) Kind() string
```

Kind returns the stream's kind.

#### func (*Stream[K, E]) Types {#Stream.Types}

```go
func (s *Stream[K, E]) Types() []string
```

Types returns the names of the event types defined on the stream, in the order they were defined.

### type StreamID {#StreamID}

```go
type StreamID[K Key, E any] struct {
	// contains filtered or unexported fields
}
```

StreamID names one stream: its kind and its key.

#### func (StreamID[K, E]) Key {#StreamID.Key}

```go
func (id StreamID[K, E]) Key() K
```

Key returns the stream's key.

#### func (StreamID[K, E]) Stream {#StreamID.Stream}

```go
func (id StreamID[K, E]) Stream() *Stream[K, E]
```

Stream returns the stream's kind.

### type Type {#Type}

```go
type Type[K Key, E, T any] struct {
	// contains filtered or unexported fields
}
```

Type is an event type of a stream: the Go type T, stored under a name.

#### func Define {#Define}

```go
func Define[K Key, E, T any](s *Stream[K, E], name string, as func(T) E) Type[K, E, T]
```

Define defines T as an event type of s, stored under name. as converts a T to the stream's event type; passing it makes the compiler check that T is one. Go cannot constrain T by E, so each stream gets a helper of one line that does:

```go
func defineCard[T CardEvent](name string) events.Type[int64, CardEvent, T] {
	return events.Define(Cards, name, func(t T) CardEvent { return t })
}

var Renamed = defineCard[CardRenamed]("card.renamed")
```

Define panics when the name or T is defined on s already: define each type once, at package level.

#### func (Type[K, E, T]) Name {#Type.Name}

```go
func (t Type[K, E, T]) Name() string
```

Name returns the name the tables store the type under.

### type Upcaster {#Upcaster}

```go
type Upcaster struct {
	// contains filtered or unexported fields
}
```

Upcaster turns a stored event of an older type into the newer type as it is read. Make it with Upcast and pass it to New.

#### func Upcast {#Upcast}

```go
func Upcast[K Key, E, Old, New any](from Type[K, E, Old], to Type[K, E, New], fn func(Old) New) Upcaster
```

Upcast reads every stored event of type from as an event of type to, converted by fn. Stored events are never changed, but for the snapshots that Compact writes: an event shape that needs more than a new optional field (which old events decode without) gets a new type, and the old type stays defined for the events stored with it. Projectors and deciders then see only the new type, and a command can no longer append the old one. Upcasters chain: from v1 to v2 and from v2 to v3 reads v1 as v3.

#### func (Upcaster) From {#Upcaster.From}

```go
func (u Upcaster) From() string
```

From returns the name of the type it reads.

#### func (Upcaster) To {#Upcaster.To}

```go
func (u Upcaster) To() string
```

To returns the name of the type it turns it into.
