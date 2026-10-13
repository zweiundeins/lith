// Package writer is the single writer of a SQLite database. Commands queue
// up while a batch runs, and the next batch takes all of them (up to
// MaxBatch, without waiting for more) into one transaction with a
// savepoint per command: under load one commit, and one fsync, covers many
// commands, and a command that fails undoes only its own changes.
// Commands are applied in the order they arrive in the queue, from Send
// and Do alike.
//
//	w, err := writer.New(db, writer.Options{OnCommit: views.Changed})
//	go w.Run(ctx)
//	defer w.Close()
//	err = w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
//		_, err := tx.Exec("UPDATE cards SET list_id = ? WHERE id = ?", list, card)
//		tx.Touch("board:7")
//		return err
//	}))
package writer

import (
	"container/list"
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zweiundeins/lith/sqlite"
	driver "modernc.org/sqlite"
)

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

// A Command is one unit of work. Apply runs inside the batch's
// transaction, in a savepoint of its own: an error, or a panic, undoes
// exactly its changes. Every queued command waits while it runs, so it
// must not block on anything but the database, and must not call Do.
type Command interface {
	Apply(*Tx) error
}

// Func is a Command made of a function.
type Func func(*Tx) error

// Apply calls f.
func (f Func) Apply(tx *Tx) error { return f(tx) }

// Validator is implemented by commands that can be checked before they
// are queued. Send and Do return its error wrapped in ErrInvalid.
type Validator interface {
	Validate() error
}

// Options tune the writer; zero fields take the defaults.
type Options struct {
	// MaxBatch caps the commands one transaction takes (default 512).
	MaxBatch int
	// QueueSize is how many commands may wait (default 4096).
	QueueSize int
	// OnCommit is called on the writer's goroutine once per committed
	// batch, with the topics its successful commands touched, sorted and
	// without duplicates (nil if none). It runs after the batch's
	// AfterCommit functions and before any Do returns. It must not block.
	OnCommit func(topics []string)
	// OptimizeEvery is how often the writer runs PRAGMA optimize between
	// batches (default 6h, negative: only on Close).
	OptimizeEvery time.Duration
	// CheckpointPages is how many pages the batches may write to the WAL
	// before the writer checkpoints it (default 1000). It applies only
	// when the database was opened with sqlite.CheckpointWriter.
	CheckpointPages int
	// Log gets panics, failed batches, failed Sends without OnSendError
	// and maintenance errors (default slog.Default()).
	Log *slog.Logger
	// OnSendError, if set, gets every command from Send that failed, with
	// its error: its own, its panic (ErrPanic) or its batch's failure. It
	// is called on the writer's goroutine once the batch has ended, after
	// OnCommit, and must not block. Nil: a command's own error is logged.
	OnSendError func(cmd Command, err error)
}

// Stats are the writer's counters since New.
type Stats struct {
	Batches    int64         // transactions, committed or not
	Commands   int64         // commands run, failed ones included
	Failures   int64         // commands that returned an error or panicked, or whose batch failed
	MaxBatch   int           // the most commands one batch took
	Queue      int           // commands waiting now
	LastCommit time.Duration // the last batch, from BEGIN to the end of COMMIT
	P99Commit  time.Duration // 99th percentile of the last 1024 batches
	Busy       time.Duration // time spent in batches
}

type request struct {
	cmd  Command    // nil for Flush and Exclusive
	ex   *exclusive // Exclusive's
	ctx  context.Context
	done chan error // nil for Send
}

// exclusive is an Exclusive's fn. Whoever takes it first decides: the
// writer to run it, or the caller to give up waiting.
type exclusive struct {
	fn    func(context.Context, *sql.Conn) error
	taken atomic.Bool
}

// Writer owns the database's write connection: it is the only one that
// writes to it after the migrations.
type Writer struct {
	conn  *sql.Conn
	opts  Options
	mode  sqlite.Checkpoint
	busy  string // the PRAGMA that restores the connection's busy timeout
	queue chan *request

	mu      sync.RWMutex // held for reading while a command goes into the queue
	closed  bool
	started atomic.Bool
	done    chan struct{}
	err     error // set before done is closed

	// The writer's goroutine alone uses these.
	stmts    map[string]*list.Element // of *stmt in lru, by SQL text
	lru      list.List                // the statements, the one used last first
	retired  []*sql.Stmt              // see closeRetired
	batch    []*request
	errs     []error
	topics   map[string]struct{}
	after    []func()
	pages    int  // WAL pages written since the last checkpoint
	dirty    bool // WAL pages written since the last TRUNCATE checkpoint
	opsReady bool // lith_ops exists
	resReady bool // lith_results exists

	batches, commands, failures, maxBatch, busyNanos atomic.Int64
	times                                            durations
}

// New takes the write connection of db and keeps it until the writer
// stops. Run applies the commands.
func New(db *sqlite.DB, opts Options) (*Writer, error) {
	if opts.MaxBatch <= 0 {
		opts.MaxBatch = 512
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 4096
	}
	if opts.OptimizeEvery == 0 {
		opts.OptimizeEvery = 6 * time.Hour
	}
	if opts.CheckpointPages <= 0 {
		opts.CheckpointPages = 1000
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if db.W == nil {
		return nil, errors.New("writer: the database has no write connection (sqlite.Options.ReadOnly)")
	}
	dbo := db.Options()
	ctx, cancel := context.WithTimeout(context.Background(), dbo.BusyTimeout)
	defer cancel()
	conn, err := db.W.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("writer: take the write connection (one Writer per database): %w", err)
	}
	return &Writer{
		conn:   conn,
		opts:   opts,
		mode:   dbo.Checkpoint,
		busy:   "PRAGMA busy_timeout = " + strconv.FormatInt(dbo.BusyTimeout.Milliseconds(), 10),
		queue:  make(chan *request, opts.QueueSize),
		done:   make(chan struct{}),
		stmts:  map[string]*list.Element{},
		topics: map[string]struct{}{},
	}, nil
}

// Run applies batches until Close is called or ctx ends. Either way it
// first applies every command already accepted, then runs PRAGMA optimize
// and gives the connection back. Send and Do return ErrClosed from the
// moment it stops taking commands: to keep taking them while an HTTP
// server drains, pass a ctx that outlives the server and call Close.
func (w *Writer) Run(ctx context.Context) error {
	if !w.started.CompareAndSwap(false, true) {
		return errors.New("writer: Run called twice, or after Close")
	}
	w.loop(ctx)
	return w.err
}

// Close stops taking commands and returns once the queued ones are
// applied and the connection is given back. Without Run, Close applies
// them itself.
func (w *Writer) Close() error {
	if w.started.CompareAndSwap(false, true) {
		go w.loop(context.Background())
	}
	w.shut()
	<-w.done
	return w.err
}

func (w *Writer) shut() {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()
}

// Send queues cmd and returns at once: ErrQueueFull if there is no room.
// Nobody waits for its outcome, so a failure goes to OnSendError, or to
// the log.
func (w *Writer) Send(cmd Command) error {
	if err := validate(cmd); err != nil {
		return err
	}
	return w.enqueue(nil, &request{cmd: cmd, ctx: context.Background()})
}

// Do queues cmd, waiting for room until ctx ends, and returns cmd's own
// error once its batch has committed. A queued command always runs, so
// once it is queued Do reports its outcome even if ctx ends meanwhile:
// a caller never mistakes a committed change for a failed one. The
// command's Tx.Context carries ctx's values but is never cancelled.
func (w *Writer) Do(ctx context.Context, cmd Command) error {
	if err := validate(cmd); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r := &request{cmd: cmd, ctx: context.WithoutCancel(ctx), done: make(chan error, 1)}
	if err := w.enqueue(ctx, r); err != nil {
		return err
	}
	return <-r.done
}

// DoValue runs fn as a command, like Do, and returns what fn returned once
// its batch has committed, or the zero value and the error.
//
//	id, err := w.DoValue(ctx, func(tx *writer.Tx) (int64, error) {
//		res, err := tx.Exec(`INSERT INTO notes (body) VALUES (?)`, body)
//		if err != nil {
//			return 0, err
//		}
//		return res.LastInsertId()
//	})
func (w *Writer) DoValue[T any](ctx context.Context, fn func(*Tx) (T, error)) (T, error) {
	var v T
	err := w.Do(ctx, Func(func(tx *Tx) (err error) {
		v, err = fn(tx)
		return err
	}))
	if err != nil {
		var zero T
		return zero, err
	}
	return v, nil
}

// Flush returns once every command queued before it has been applied,
// committed or failed: a failure goes to the command's Do, or for a Send
// to OnSendError. Since commands are applied in the order they arrive, a read
// after Flush sees what the commands sent before it committed. Flush
// returns ctx's error if ctx ends first, and ErrClosed once the writer
// has stopped taking commands.
func (w *Writer) Flush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r := &request{done: make(chan error, 1)}
	if err := w.enqueue(ctx, r); err != nil {
		return err
	}
	select {
	case err := <-r.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Exclusive runs fn on the writer's connection between two batches,
// outside any transaction, and returns fn's error. Commands queued
// meanwhile wait, and run after it in the order they arrived. It is for
// work a command's transaction cannot hold, or that must not race the
// writer: a PRAGMA foreign_keys toggle, ATTACH and DETACH, VACUUM, a long
// import.
//
// fn must restore every pragma it changes, also when it fails, and end
// what it begins: a transaction it leaves open is rolled back, and
// Exclusive reports it. It must not close conn or keep it, nor wait for
// the writer (Do, Flush, Exclusive). fn may change the schema: the writer
// prepares its statements again after it. Its changes reach no OnCommit,
// so tell the views once Exclusive returns.
//
//	err := w.Exclusive(ctx, func(ctx context.Context, conn *sql.Conn) error {
//		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
//			return err
//		}
//		defer conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA foreign_keys = ON")
//		_, err := conn.ExecContext(ctx, "DELETE FROM accounts")
//		return err
//	})
//
// fn gets ctx. If ctx ends before fn's turn, fn does not run and
// Exclusive returns ctx's error; once fn has begun, Exclusive waits for
// it. A panic in fn is returned as ErrPanic.
func (w *Writer) Exclusive(ctx context.Context, fn func(ctx context.Context, conn *sql.Conn) error) error {
	if fn == nil {
		return fmt.Errorf("%w: nil", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r := &request{ex: &exclusive{fn: fn}, ctx: ctx, done: make(chan error, 1)}
	if err := w.enqueue(ctx, r); err != nil {
		return err
	}
	select {
	case err := <-r.done:
		return err
	case <-ctx.Done():
		if r.ex.taken.CompareAndSwap(false, true) {
			return ctx.Err()
		}
		return <-r.done
	}
}

func validate(cmd Command) error {
	if cmd == nil {
		return fmt.Errorf("%w: nil", ErrInvalid)
	}
	if v, ok := cmd.(Validator); ok {
		if err := v.Validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	return nil
}

// enqueue waits for room until ctx ends; with a nil ctx it does not wait.
func (w *Writer) enqueue(ctx context.Context, r *request) error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return ErrClosed
	}
	if ctx == nil {
		select {
		case w.queue <- r:
			return nil
		default:
			return ErrQueueFull
		}
	}
	select {
	case w.queue <- r:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats reads the counters.
func (w *Writer) Stats() Stats {
	last, p99 := w.times.get()
	return Stats{
		Batches:    w.batches.Load(),
		Commands:   w.commands.Load(),
		Failures:   w.failures.Load(),
		MaxBatch:   int(w.maxBatch.Load()),
		Queue:      len(w.queue),
		LastCommit: last,
		P99Commit:  p99,
		Busy:       time.Duration(w.busyNanos.Load()),
	}
}

func (w *Writer) loop(ctx context.Context) {
	defer close(w.done)
	stop := ctx.Done()
	var optimize <-chan time.Time
	if w.opts.OptimizeEvery > 0 {
		t := time.NewTicker(w.opts.OptimizeEvery)
		defer t.Stop()
		optimize = t.C
	}
	idle := time.NewTimer(time.Second)
	idle.Stop()
	defer idle.Stop()
	for {
		select {
		case r, ok := <-w.queue:
			if !ok {
				w.err = w.finish()
				return
			}
			// r is the last request taken. An Exclusive ends the batch,
			// and runs once the commands before it have committed.
			w.batch = append(w.batch[:0], r)
		fill:
			for len(w.batch) < w.opts.MaxBatch && r.ex == nil {
				select {
				case next, ok := <-w.queue:
					if !ok {
						break fill
					}
					w.batch = append(w.batch, next)
					r = next
				default:
					break fill
				}
			}
			if r.ex != nil {
				w.apply(w.batch[:len(w.batch)-1])
				w.lend(r)
			} else {
				w.apply(w.batch)
			}
			clear(w.batch)
			if w.mode == sqlite.CheckpointWriter {
				w.checkpoint()
				idle.Reset(time.Second)
			}
		case <-stop:
			stop = nil
			// The queue closes once blocked senders are in; until then
			// this loop has to keep taking their commands.
			go w.shut()
		case <-idle.C:
			if !w.truncate() {
				idle.Reset(time.Second)
			}
		case <-optimize:
			if err := w.exec("PRAGMA optimize"); err != nil {
				w.opts.Log.Warn("writer: optimize", "err", err)
			}
		}
	}
}

func (w *Writer) apply(batch []*request) {
	n := int64(0)
	for _, r := range batch {
		if r.cmd != nil {
			n++
		}
	}
	if n == 0 {
		for _, r := range batch {
			r.done <- nil
		}
		return
	}
	start := time.Now()
	if cap(w.errs) < len(batch) {
		w.errs = make([]error, len(batch))
	}
	errs := w.errs[:len(batch)]
	var failed error
	committed := false
	if err := w.exec("BEGIN IMMEDIATE"); err != nil {
		failed = fmt.Errorf("writer: begin: %w", err)
	} else {
		for i, r := range batch {
			if r.cmd == nil {
				continue
			}
			if errs[i], failed = w.one(r); failed != nil {
				break
			}
		}
		if failed == nil {
			if err := w.exec("COMMIT"); err != nil {
				failed = fmt.Errorf("writer: commit: %w", err)
			} else {
				committed = true
			}
		}
	}
	if !committed {
		_ = w.exec("ROLLBACK") // a failed COMMIT can leave the transaction open
	}

	elapsed := time.Since(start)
	w.batches.Add(1)
	w.commands.Add(n)
	w.busyNanos.Add(int64(elapsed))
	if n > w.maxBatch.Load() {
		w.maxBatch.Store(n)
	}
	w.times.add(elapsed)
	if failed != nil {
		w.opts.Log.Error("writer: batch failed", "commands", n, "err", failed)
		for i, r := range batch {
			if errs[i] == nil && r.cmd != nil {
				errs[i] = failed
			}
		}
	}
	var nfailed int64
	for _, err := range errs {
		if err != nil {
			nfailed++
		}
	}
	w.failures.Add(nfailed)

	if committed {
		for _, fn := range w.after {
			w.safely("AfterCommit", fn)
		}
		if w.opts.OnCommit != nil {
			var topics []string
			if len(w.topics) > 0 {
				topics = make([]string, 0, len(w.topics))
				for t := range w.topics {
					topics = append(topics, t)
				}
				slices.Sort(topics)
			}
			w.safely("OnCommit", func() { w.opts.OnCommit(topics) })
		}
	}
	clear(w.topics)
	clear(w.after)
	w.after = w.after[:0]

	for i, r := range batch {
		err := errs[i]
		switch {
		case r.done != nil:
			r.done <- err
		case err == nil:
		case w.opts.OnSendError != nil:
			w.safely("OnSendError", func() { w.opts.OnSendError(r.cmd, err) })
		case err != failed && !errors.Is(err, ErrPanic):
			w.opts.Log.Warn("writer: command failed", "cmd", fmt.Sprintf("%T", r.cmd), "err", err)
		}
	}
	clear(errs)
}

// one runs r in a savepoint of its own and keeps its topics and
// AfterCommit functions if it succeeds. batchErr is set when the
// savepoint itself failed, which ends the batch.
func (w *Writer) one(r *request) (cmdErr, batchErr error) {
	if err := w.exec("SAVEPOINT cmd"); err != nil {
		return nil, fmt.Errorf("writer: savepoint: %w", err)
	}
	tx := &Tx{w: w, ctx: r.ctx}
	cmdErr, keep := unkept(w.call(r.cmd, tx))
	tx.end()
	if !keep {
		if err := w.exec("ROLLBACK TO cmd"); err != nil {
			return cmdErr, fmt.Errorf("writer: roll back to savepoint: %w", err)
		}
	}
	if err := w.exec("RELEASE cmd"); err != nil {
		return cmdErr, fmt.Errorf("writer: release savepoint: %w", err)
	}
	if keep {
		for _, t := range tx.topics {
			w.topics[t] = struct{}{}
		}
		w.after = append(w.after, tx.after...)
	}
	return cmdErr, nil
}

// kept is the error of a command whose changes stay all the same:
// OnceResult's record of a refusal.
type kept struct{ err error }

func (k *kept) Error() string { return k.err.Error() }
func (k *kept) Unwrap() error { return k.err }

// unkept returns the error a command reports and whether its changes
// stay.
func unkept(err error) (error, bool) {
	if k, ok := err.(*kept); ok {
		return k.err, true
	}
	return err, err == nil
}

// call runs cmd and turns a panic into its error: a broken command fails
// alone instead of taking the writer, and every queued command, down.
func (w *Writer) call(cmd Command, tx *Tx) (err error) {
	defer func() {
		if p := recover(); p != nil {
			w.opts.Log.Error("writer: command panicked", "cmd", fmt.Sprintf("%T", cmd), "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("%w: %v", ErrPanic, p)
		}
	}()
	return cmd.Apply(tx)
}

// lend runs an Exclusive's fn on the connection, unless its caller gave
// up waiting, and answers it.
func (w *Writer) lend(r *request) {
	if !r.ex.taken.CompareAndSwap(false, true) {
		return
	}
	if err := r.ctx.Err(); err != nil {
		r.done <- err
		return
	}
	r.done <- w.runExclusive(r.ctx, r.ex.fn)
}

func (w *Writer) runExclusive(ctx context.Context, fn func(context.Context, *sql.Conn) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			w.opts.Log.Error("writer: Exclusive panicked", "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("%w: %v", ErrPanic, p)
		}
		// Without a transaction open, ROLLBACK fails and changes nothing.
		if _, e := w.conn.ExecContext(context.Background(), "ROLLBACK"); e == nil {
			err = errors.Join(err, errors.New("writer: Exclusive left a transaction open, rolled back"))
		}
		w.forget()
	}()
	return fn(ctx, w.conn)
}

// forget closes the prepared statements and drops what the writer knows
// of the schema, which an Exclusive may have changed.
func (w *Writer) forget() {
	for _, e := range w.stmts {
		e.Value.(*stmt).s.Close()
	}
	clear(w.stmts)
	w.lru.Init()
	w.closeRetired()
	w.opsReady, w.resReady = false, false
}

func (w *Writer) safely(what string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			w.opts.Log.Error("writer: "+what+" panicked", "panic", p, "stack", string(debug.Stack()))
		}
	}()
	fn()
}

// checkpoint copies the WAL into the database once the batches since the
// last checkpoint wrote CheckpointPages pages. PASSIVE never waits: what
// a reader still uses stays in the WAL for the next one.
func (w *Writer) checkpoint() {
	n, err := w.walPages()
	if err != nil {
		w.opts.Log.Warn("writer: read the WAL size", "err", err)
		return
	}
	if n == 0 {
		return
	}
	w.dirty = true
	if w.pages += n; w.pages < w.opts.CheckpointPages {
		return
	}
	w.pages = 0
	if err := w.exec("PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
		w.opts.Log.Warn("writer: checkpoint", "err", err)
	}
}

// walPages returns the pages the connection wrote to the WAL since the
// last call. A wrapping driver (sqlite.Options.Driver) hands out its own
// connection, which must Unwrap to modernc's.
func (w *Writer) walPages() (n int, err error) {
	err = w.conn.Raw(func(dc any) error {
		for {
			if s, ok := dc.(driver.DBStatus); ok {
				n, _, err = s.Status(driver.DBStatusCacheWrite, true)
				return err
			}
			u, ok := dc.(interface{ Unwrap() sqldriver.Conn })
			if !ok {
				return fmt.Errorf("%T has no DBStatus", dc)
			}
			dc = u.Unwrap()
		}
	})
	return n, err
}

// truncate runs once no command came for a second: it checkpoints the
// whole WAL and truncates the file. It does not wait for readers; while
// one still reads from the WAL it reports false, to be tried again.
func (w *Writer) truncate() bool {
	if !w.dirty {
		return true
	}
	if err := w.exec("PRAGMA busy_timeout = 0"); err != nil {
		w.opts.Log.Warn("writer: checkpoint", "err", err)
		return false
	}
	var busy, wal, copied int
	err := w.row("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &wal, &copied)
	if e := w.exec(w.busy); e != nil {
		err = errors.Join(err, e)
	}
	if err != nil {
		w.opts.Log.Warn("writer: checkpoint", "err", err)
		return false
	}
	if busy != 0 {
		return false
	}
	w.dirty, w.pages = false, 0
	return true
}

func (w *Writer) finish() error {
	var errs []error
	if err := w.exec("PRAGMA optimize"); err != nil {
		errs = append(errs, fmt.Errorf("writer: optimize: %w", err))
	}
	w.forget()
	if err := w.conn.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// durations keeps the last batches' times.
type durations struct {
	mu sync.Mutex
	d  [1024]time.Duration
	n  int
}

func (r *durations) add(d time.Duration) {
	r.mu.Lock()
	r.d[r.n%len(r.d)] = d
	r.n++
	r.mu.Unlock()
}

func (r *durations) get() (last, p99 time.Duration) {
	r.mu.Lock()
	n := min(r.n, len(r.d))
	if n == 0 {
		r.mu.Unlock()
		return 0, 0
	}
	last = r.d[(r.n-1)%len(r.d)]
	s := slices.Clone(r.d[:n])
	r.mu.Unlock()
	slices.Sort(s)
	return last, s[(n*99+99)/100-1]
}
