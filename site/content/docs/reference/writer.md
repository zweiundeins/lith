---
title: writer
headline: lith/writer: the single writer of a SQLite database
summary: The single writer of a SQLite database.
description: Reference for lith/writer: the one writer of a SQLite database, which applies commands in batches of one transaction with a savepoint each.
order: 2
---

Import path `github.com/zweiundeins/lith/writer`. Generated from the doc comments in [writer/](https://github.com/zweiundeins/lith/tree/main/writer).

Commands queue up while a batch runs, and the next batch takes all of them (up to MaxBatch, without waiting for more) into one transaction with a savepoint per command: under load one commit, and one fsync, covers many commands, and a command that fails undoes only its own changes. A batch of one command needs no savepoint: when it fails, its transaction rolls back. Commands are applied in the order they arrive in the queue, from Send and Do alike.

```go
w, err := writer.New(db, writer.Options{OnCommit: views.Changed})
go w.Run(ctx)
defer w.Close()
err = w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
	_, err := tx.Exec("UPDATE cards SET list_id = ? WHERE id = ?", list, card)
	tx.Touch("board:7")
	return err
}))
```

## Constants

```go
const SizeBuckets = 11
```

SizeBuckets is the number of buckets in Stats.Sizes.

## Variables

```go
var (
	// ErrClosed is returned for a command sent after the writer stopped.
	ErrClosed = errors.New("writer: closed")
	// ErrQueueFull is returned by Send when the queue has no room.
	ErrQueueFull = errors.New("writer: queue full")
	// ErrInvalid wraps the error of a command that failed Validate.
	ErrInvalid = errors.New("writer: invalid command")
	// ErrPanic wraps the value a command, or an Exclusive's fn, panicked with.
	ErrPanic = errors.New("writer: command panicked")
)
```

## Types

### type Command {#Command}

```go
type Command interface {
	Apply(*Tx) error
}
```

A Command is one unit of work. Apply runs inside the batch's transaction, in a savepoint of its own: an error, or a panic, undoes exactly its changes. Every queued command waits while it runs, so it must not block on anything but the database, and must not call Do.

#### func Once {#Once}

```go
func Once(key string, cmd Command) Command
```

Once makes cmd idempotent per key, such as an op id from the page scoped to its user, so that a retried request gets the first answer. When cmd succeeds, Once records the key in the table lith\_ops (created on demand, in the same savepoint); a command with a recorded key succeeds without running cmd again. A failed command changed nothing and leaves no record: a retry runs it again and gets its error afresh, with its type. PruneOnce forgets old keys.

#### func PruneOnce {#PruneOnce}

```go
func PruneOnce(age time.Duration) Command
```

PruneOnce returns a command that forgets the keys Once and OnceResult recorded more than age ago. Send it now and then, hourly with an age of a day, say.

### type Func {#Func}

```go
type Func func(*Tx) error
```

Func is a Command made of a function.

#### func (Func) Apply {#Func.Apply}

```go
func (f Func) Apply(tx *Tx) error
```

Apply calls f.

### type Op {#Op}

```go
type Op struct {
	// contains filtered or unexported fields
}
```

Op is a command made by OnceResult. Read its answer once Do returned.

#### func OnceResult {#OnceResult}

```go
func OnceResult(key string, cmd ResultCommand) *Op
```

OnceResult is Once for a command with an answer: it records the key with the command's result, or with its refusal, an error that is a \*Refusal (errors.As). A refusal undoes the command's changes, as any error does, but its record stays. A command with a recorded key does not run again: Do returns the recorded refusal, as a \*Refusal with its message, and Result the recorded result. Other errors leave no record, so a retry runs the command again. PruneOnce forgets old keys.

#### func (*Op) Apply {#Op.Apply}

```go
func (o *Op) Apply(tx *Tx) error
```

Apply runs the command unless its key is recorded, and records its answer.

#### func (*Op) Replayed {#Op.Replayed}

```go
func (o *Op) Replayed() bool
```

Replayed reports whether the answer is a recorded one, from an earlier run with the key.

#### func (*Op) Result {#Op.Result}

```go
func (o *Op) Result() []byte
```

Result is the command's result, from this run or a recorded one.

#### func (*Op) Validate {#Op.Validate}

```go
func (o *Op) Validate() error
```

Validate checks the key and the command, and runs the command's own Validate.

### type Options {#Options}

```go
type Options struct {
	// MaxBatch caps the commands one transaction takes (default 512).
	MaxBatch int
	// QueueSize is how many commands may wait (default 4096).
	QueueSize int
	// OnCommit is called on the writer's goroutine once per committed
	// batch, with the topics its successful commands touched, sorted and
	// without duplicates (nil if none). It runs after the batch's
	// AfterCommit functions, before the values its commands published
	// reach the subscribers, and before any Do returns. It must not block.
	OnCommit func(topics []string)
	// OptimizeEvery is how often the writer runs PRAGMA optimize between
	// batches (default 6h, negative: only on Close).
	OptimizeEvery time.Duration
	// CheckpointPages is how many pages the batches may write to the WAL
	// before the writer checkpoints it (default 1000). It applies only
	// when the database was opened with sqlite.CheckpointWriter.
	CheckpointPages int
	// CheckpointAside runs those checkpoints on a connection of their own
	// (sqlite.DB.Checkpointer), in a goroutine of their own, instead of on
	// the write connection between batches: after a large write the next
	// batch does not wait for the checkpoint that copies it. The writer
	// still counts the pages and truncates the WAL when idle. It applies
	// only with sqlite.CheckpointWriter.
	CheckpointAside bool
	// SlowCommand is how long a command may run before the writer logs
	// its type, once per command, and again when it ends (default 5s,
	// negative: never). Every other command waits meanwhile, so a command
	// that blocks on something besides the database stops the writer;
	// Stats.Running shows one under way.
	SlowCommand time.Duration
	// Log gets panics, failed batches, failed Sends without OnSendError
	// and maintenance errors (default slog.Default()).
	Log *slog.Logger
	// OnSendError, if set, gets every command from Send that failed, with
	// its error: its own, its panic (ErrPanic) or its batch's failure. It
	// is called on the writer's goroutine once the batch has ended, after
	// OnCommit, and must not block. Nil: a command's own error is logged.
	OnSendError func(cmd Command, err error)
}
```

Options tune the writer; zero fields take the defaults.

### type Refusal {#Refusal}

```go
type Refusal struct{ Msg string }
```

Refusal is a command's refusal, with the message the user reads. OnceResult records it like a result.

#### func (*Refusal) Error {#Refusal.Error}

```go
func (r *Refusal) Error() string
```

### type ResultCommand {#ResultCommand}

```go
type ResultCommand interface {
	ApplyResult(*Tx) ([]byte, error)
}
```

ResultCommand is a command with an answer besides its error: a small result, such as the id of a row it created or an outcome as JSON.

### type ResultFunc {#ResultFunc}

```go
type ResultFunc func(*Tx) ([]byte, error)
```

ResultFunc is a ResultCommand made of a function.

#### func (ResultFunc) ApplyResult {#ResultFunc.ApplyResult}

```go
func (f ResultFunc) ApplyResult(tx *Tx) ([]byte, error)
```

ApplyResult calls f.

### type Stats {#Stats}

```go
type Stats struct {
	Batches     int64         // transactions, committed or not
	Commands    int64         // commands run, failed ones included
	Failures    int64         // commands that returned an error or panicked, or whose batch failed
	MaxBatch    int           // the most commands one batch took
	Queue       int           // commands waiting now
	LastCommit  time.Duration // the last batch, from BEGIN to the end of COMMIT
	P99Commit   time.Duration // 99th percentile of the last 1024 batches
	Busy        time.Duration // time spent in batches
	Running     time.Duration // how long the command under way has run, 0 between commands
	Published   int64         // values committed commands published (Tx.Publish)
	Dropped     int64         // values a subscriber's full queue did not take, once per subscriber
	Subscribers int           // subscriptions open now (Writer.Subscribe)
	// Sizes counts the batches by the commands they took, in powers of
	// two: Sizes[0] those of one command, Sizes[i] those of 2^i to
	// 2^(i+1)-1, the last those of 1024 and more. They add up to Batches.
	Sizes [SizeBuckets]int64
}
```

Stats are the writer's counters since New.

### type Tx {#Tx}

```go
type Tx struct {
	// contains filtered or unexported fields
}
```

Tx is what a Command gets: the batch's transaction on the writer's connection. Its statements are prepared once, on that connection, and kept by their SQL text, the 512 used last. Build queries from constants and pass values as arguments; SQL built at runtime, such as an IN list with a mark per value, runs through ExecUncached, QueryUncached and QueryRowUncached, which keep no statement, as do ExecContext, QueryContext and QueryRowContext with a ctx from sqlite.Uncached.

#### func (*Tx) AfterCommit {#Tx.AfterCommit}

```go
func (t *Tx) AfterCommit(fn func())
```

AfterCommit runs fn on the writer's goroutine once the batch has committed, before Do returns, in the order the commands registered their functions. If the command fails, or the savepoint fn was registered in, fn is dropped. fn must not block.

#### func (*Tx) Context {#Tx.Context}

```go
func (t *Tx) Context() context.Context
```

Context returns the context of the Do that queued the command, never cancelled, or context.Background() for Send.

#### func (*Tx) Exec {#Tx.Exec}

```go
func (t *Tx) Exec(query string, args ...any) (sql.Result, error)
```

Exec runs a statement.

#### func (*Tx) ExecContext {#Tx.ExecContext}

```go
func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
```

ExecContext is Exec, for code that takes a sqlite.Querier. Like the other methods it runs on the batch's transaction, with its statement cache and the command's context: ctx is not used, so no statement is cancelled halfway through a batch, except that with a ctx from sqlite.Uncached it is ExecUncached.

#### func (*Tx) ExecUncached {#Tx.ExecUncached}

```go
func (t *Tx) ExecUncached(query string, args ...any) (sql.Result, error)
```

ExecUncached is Exec for SQL built at runtime, such as an IN list with a mark per value: the statement is prepared for this call only, instead of taking a place in the cache for each length of the list.

#### func (*Tx) Local {#Tx.Local}

```go
func (t *Tx) Local(key any) any
```

Local returns the value SetLocal stored under key on this writer, or nil. Packages built on the writer keep there what they know of its database, such as whether their tables exist: one package-level value can then serve the databases of many writers, a database per account among them. Commands run one at a time, so Local needs no lock.

#### func (*Tx) Publish {#Tx.Publish}

```go
func (t *Tx) Publish(v any)
```

Publish hands v to the writer's subscribers (Writer.Subscribe) once the batch has committed, after its AfterCommit functions and OnCommit. Like an AfterCommit function, v is dropped if the command fails, or the savepoint it was published in. Delivery is best effort: see Subscribe. A nil v is ignored.

#### func (*Tx) Query {#Tx.Query}

```go
func (t *Tx) Query(query string, args ...any) (*sql.Rows, error)
```

Query runs a query. While its rows are open, the same query runs on a statement of its own. Rows still open when the command returns are closed then.

#### func (*Tx) QueryContext {#Tx.QueryContext}

```go
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
```

QueryContext is Query, for code that takes a sqlite.Querier; ctx is not used, except that with a ctx from sqlite.Uncached it is QueryUncached.

#### func (*Tx) QueryRow {#Tx.QueryRow}

```go
func (t *Tx) QueryRow(query string, args ...any) *sql.Row
```

QueryRow runs a query for one row. Scan the row before running the same query again: until then its statement is busy.

#### func (*Tx) QueryRowContext {#Tx.QueryRowContext}

```go
func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
```

QueryRowContext is QueryRow, for code that takes a sqlite.Querier; ctx is not used, except that with a ctx from sqlite.Uncached it is QueryRowUncached.

#### func (*Tx) QueryRowUncached {#Tx.QueryRowUncached}

```go
func (t *Tx) QueryRowUncached(query string, args ...any) *sql.Row
```

QueryRowUncached is QueryRow for SQL built at runtime, without the statement cache.

#### func (*Tx) QueryUncached {#Tx.QueryUncached}

```go
func (t *Tx) QueryUncached(query string, args ...any) (*sql.Rows, error)
```

QueryUncached is Query for SQL built at runtime, without the statement cache. Rows still open when the command returns are closed then.

#### func (*Tx) Savepoint {#Tx.Savepoint}

```go
func (t *Tx) Savepoint(fn func(*Tx) error) error
```

Savepoint runs fn in a nested savepoint. If fn returns an error or panics, its changes are rolled back, the topics it touched, the functions it registered and the values it published are dropped, and its error is returned: the command can go on.

#### func (*Tx) SetLocal {#Tx.SetLocal}

```go
func (t *Tx) SetLocal(key, value any)
```

SetLocal stores value under key on this writer, for the commands that follow, until the writer stops. It is not undone when the command fails: set what depends on the commit in AfterCommit.

#### func (*Tx) Touch {#Tx.Touch}

```go
func (t *Tx) Touch(topics ...string)
```

Touch records that the command changed what the topics name. If the command succeeds, OnCommit gets them after the batch commits.

#### func (*Tx) Untouched {#Tx.Untouched}

```go
func (t *Tx) Untouched(fn func(*Tx) error) error
```

Untouched runs fn and drops the topics it touched, while its changes, its AfterCommit functions and its published values stay: for writes that no view reads yet, such as a table being filled to replace another. It returns fn's error and undoes nothing; run fn in a Savepoint for that.

### type Validator {#Validator}

```go
type Validator interface {
	Validate() error
}
```

Validator is implemented by commands that can be checked before they are queued. Send and Do return its error wrapped in ErrInvalid.

### type Writer {#Writer}

```go
type Writer struct {
	// contains filtered or unexported fields
}
```

Writer owns the database's write connection: it is the only one that writes to it after the migrations.

#### func New {#New}

```go
func New(db *sqlite.DB, opts Options) (*Writer, error)
```

New takes the write connection of db and keeps it until the writer stops. Run applies the commands.

#### func (*Writer) Close {#Writer.Close}

```go
func (w *Writer) Close() error
```

Close stops taking commands and returns once the queued ones are applied and the connection is given back. Without Run, Close applies them itself.

#### func (*Writer) Do {#Writer.Do}

```go
func (w *Writer) Do(ctx context.Context, cmd Command) error
```

Do queues cmd, waiting for room until ctx ends, and returns cmd's own error once its batch has committed. A queued command always runs, so once it is queued Do reports its outcome even if ctx ends meanwhile: a caller never mistakes a committed change for a failed one. The command's Tx.Context carries ctx's values but is never cancelled.

#### func (*Writer) DoValue {#Writer.DoValue}

```go
func (w *Writer) DoValue[T any](ctx context.Context, fn func(*Tx) (T, error)) (T, error)
```

DoValue runs fn as a command, like Do, and returns what fn returned once its batch has committed, or the zero value and the error.

```go
id, err := w.DoValue(ctx, func(tx *writer.Tx) (int64, error) {
	res, err := tx.Exec(`INSERT INTO notes (body) VALUES (?)`, body)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
})
```

#### func (*Writer) Exclusive {#Writer.Exclusive}

```go
func (w *Writer) Exclusive(ctx context.Context, fn func(ctx context.Context, conn *sql.Conn) error) error
```

Exclusive runs fn on the writer's connection between two batches, outside any transaction, and returns fn's error. Commands queued meanwhile wait, and run after it in the order they arrived. It is for work a command's transaction cannot hold, or that must not race the writer: a PRAGMA foreign\_keys toggle, ATTACH and DETACH, VACUUM, a long import.

fn must restore every pragma it changes, also when it fails, and end what it begins: a transaction it leaves open is rolled back, and Exclusive reports it. It must not close conn or keep it, nor wait for the writer (Do, Flush, Exclusive). fn may change the schema: the writer prepares its statements again after it. Its changes reach no OnCommit, so tell the views once Exclusive returns.

```go
err := w.Exclusive(ctx, func(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA foreign_keys = ON")
	_, err := conn.ExecContext(ctx, "DELETE FROM accounts")
	return err
})
```

fn gets ctx. If ctx ends before fn's turn, fn does not run and Exclusive returns ctx's error; once fn has begun, Exclusive waits for it. A panic in fn is returned as ErrPanic.

#### func (*Writer) Flush {#Writer.Flush}

```go
func (w *Writer) Flush(ctx context.Context) error
```

Flush returns once every command queued before it has been applied, committed or failed: a failure goes to the command's Do, or for a Send to OnSendError. Since commands are applied in the order they arrive, a read after Flush sees what the commands sent before it committed. Flush returns ctx's error if ctx ends first, and ErrClosed once the writer has stopped taking commands.

#### func (*Writer) Run {#Writer.Run}

```go
func (w *Writer) Run(ctx context.Context) error
```

Run applies batches until Close is called or ctx ends. Either way it first applies every command already accepted, then runs PRAGMA optimize and gives the connection back. Send and Do return ErrClosed from the moment it stops taking commands: to keep taking them while an HTTP server drains, pass a ctx that outlives the server and call Close.

#### func (*Writer) Send {#Writer.Send}

```go
func (w *Writer) Send(cmd Command) error
```

Send queues cmd and returns at once: ErrQueueFull if there is no room. Nobody waits for its outcome, so a failure goes to OnSendError, or to the log.

#### func (*Writer) Stats {#Writer.Stats}

```go
func (w *Writer) Stats() Stats
```

Stats reads the counters.

#### func (*Writer) Subscribe {#Writer.Subscribe}

```go
func (w *Writer) Subscribe[T any](ctx context.Context, n int) <-chan T
```

Subscribe returns a channel of the values that committed commands publish (Tx.Publish) and that are of type T: Subscribe\[any] gets all of them, an interface type the values that implement it. They arrive in commit order: batch by batch, the commands of a batch in their order, and a command's values in the order it published them. The writer hands them over on its goroutine once the batch's AfterCommit functions and OnCommit have run, before any Do of the batch returns, and never waits: the channel holds n values (at least 1), and a value that finds it full is dropped for this subscriber alone and counted in Stats.Dropped. The channel closes once ctx ends or the writer stops.

Delivery is best effort and in process. A value is lost when its subscriber's queue is full, and when the process stops between the commit and the delivery; a subscriber gets nothing published before it subscribed, and nothing is replayed. A listener that must not miss anything reconciles from the data now and then, or reads the events package's streams instead.

```go
go func() {
	for m := range w.Subscribe[CardMoved](ctx, 256) {
		notify(m.Board, m.Card)
	}
}()
```
