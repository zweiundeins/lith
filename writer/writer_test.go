package writer

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/zweiundeins/lith/sqlite"
)

const schema = `
CREATE TABLE counters (id INTEGER PRIMARY KEY, n INTEGER NOT NULL DEFAULT 0);
CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, seq INTEGER);
CREATE TABLE blobs (id INTEGER PRIMARY KEY, data BLOB NOT NULL);
INSERT INTO counters (id) VALUES (1), (2);
`

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type fixture struct {
	db  *sqlite.DB
	w   *Writer
	log *syncBuffer
}

func openDB(t testing.TB, so sqlite.Options) *sqlite.DB {
	t.Helper()
	if so.Synchronous == "" {
		so.Synchronous = "NORMAL"
	}
	so.Migrations = fstest.MapFS{"001_init.sql": {Data: []byte(schema)}}
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), so)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// start opens a database and runs a writer on it until the test ends.
func start(t testing.TB, so sqlite.Options, opts Options) *fixture {
	t.Helper()
	db := openDB(t, so)
	f := &fixture{db: db, log: &syncBuffer{}}
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(f.log, nil))
	}
	w, err := New(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	f.w = w
	go w.Run(context.Background())
	t.Cleanup(func() {
		w.Close()
		db.Close()
	})
	return f
}

func (f *fixture) count(t testing.TB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.R.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// block holds the writer in a command until release is called, so that
// what is queued meanwhile lands in one batch.
func (f *fixture) block(t testing.TB) (release func()) {
	t.Helper()
	started, gate := make(chan struct{}), make(chan struct{})
	go f.w.Do(context.Background(), Func(func(*Tx) error {
		close(started)
		<-gate
		return nil
	}))
	<-started
	return func() { close(gate) }
}

// queued waits until n commands wait in the queue.
func (f *fixture) queued(t testing.TB, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.w.Stats().Queue < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d commands queued, want %d", f.w.Stats().Queue, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func insert(name string) Func {
	return func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO items (name) VALUES (?)", name)
		return err
	}
}

func increment(id int) Func {
	return func(tx *Tx) error {
		_, err := tx.Exec("UPDATE counters SET n = n + 1 WHERE id = ?", id)
		return err
	}
}

func TestFailedCommandRollsBackAlone(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := context.Background()
	boom := errors.New("rejected")
	release := f.block(t)
	cmds := []Command{
		insert("a"),
		Func(func(tx *Tx) error {
			if err := insert("b")(tx); err != nil {
				return err
			}
			return boom
		}),
		Func(func(tx *Tx) error {
			if err := insert("c")(tx); err != nil {
				return err
			}
			panic("broken command")
		}),
		insert("d"),
	}
	errs := make([]error, len(cmds))
	var wg sync.WaitGroup
	for i, cmd := range cmds {
		wg.Go(func() { errs[i] = f.w.Do(ctx, cmd) })
		f.queued(t, i+1)
	}
	release()
	wg.Wait()
	if errs[0] != nil || !errors.Is(errs[1], boom) || !errors.Is(errs[2], ErrPanic) || errs[3] != nil {
		t.Fatalf("errors: %v", errs)
	}
	if !strings.Contains(errs[2].Error(), "broken command") {
		t.Errorf("the panic's value is missing: %v", errs[2])
	}
	var names []string
	rows, err := f.db.R.Query("SELECT name FROM items ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	if !slices.Equal(names, []string{"a", "d"}) {
		t.Fatalf("kept %v, want a and d", names)
	}
	if s := f.w.Stats(); s.MaxBatch < len(cmds) {
		t.Fatalf("want one batch of %d, stats %+v", len(cmds), s)
	}
	if log := f.log.String(); !strings.Contains(log, "broken command") || !strings.Contains(log, "writer.go") {
		t.Errorf("the panic and its stack are not in the log:\n%s", log)
	}
}

func TestOrder(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{MaxBatch: 64})
	release := f.block(t)
	const n = 1000
	for i := range n {
		cmd := Func(func(tx *Tx) error {
			_, err := tx.Exec("INSERT INTO items (name, seq) VALUES (?, ?)", fmt.Sprint("item", i), i)
			return err
		})
		if err := f.w.Send(cmd); err != nil {
			t.Fatal(err)
		}
	}
	release()
	if err := f.w.Do(context.Background(), Func(func(*Tx) error { return nil })); err != nil {
		t.Fatal(err)
	}
	rows, err := f.db.R.Query("SELECT seq FROM items ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := 0
	for rows.Next() {
		var seq int
		if err := rows.Scan(&seq); err != nil {
			t.Fatal(err)
		}
		if seq != want {
			t.Fatalf("row %d holds command %d", want, seq)
		}
		want++
	}
	if want != n {
		t.Fatalf("%d rows, want %d", want, n)
	}
	if s := f.w.Stats(); s.MaxBatch != 64 {
		t.Fatalf("largest batch %d, want MaxBatch 64", s.MaxBatch)
	}
}

// Flush returns once the commands sent before it are applied: a read
// after it sees them. It is no command itself.
func TestFlush(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := context.Background()
	if err := f.w.Flush(ctx); err != nil {
		t.Fatalf("Flush on an idle writer: %v", err)
	}
	if s := f.w.Stats(); s.Commands != 0 || s.Batches != 0 {
		t.Fatalf("a Flush counted as a command: %+v", s)
	}

	release := f.block(t)
	for range 3 {
		if err := f.w.Send(increment(1)); err != nil {
			t.Fatal(err)
		}
	}
	flushed := make(chan error, 1)
	go func() { flushed <- f.w.Flush(ctx) }()
	f.queued(t, 4)
	select {
	case err := <-flushed:
		t.Fatalf("Flush returned before the commands ran: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := f.w.Flush(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Flush past its deadline: %v", err)
	}
	release()
	if err := <-flushed; err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT n FROM counters WHERE id = 1"); n != 3 {
		t.Fatalf("after Flush the counter is %d, want the 3 sent", n)
	}
	if s := f.w.Stats(); s.Commands != 4 {
		t.Fatalf("%d commands, want the block and 3 increments", s.Commands)
	}
	f.w.Close()
	if err := f.w.Flush(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("Flush after Close: %v", err)
	}
}

// Many writers on one hot row: every increment lands, none is lost or
// refused, readers never see SQLITE_BUSY, and the batches grow.
func TestContention(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := context.Background()
	const workers, each = 64, 50
	var failed atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range each {
				if err := f.w.Do(ctx, increment(1)); err != nil {
					failed.Add(1)
				}
			}
		})
	}
	stop := make(chan struct{})
	var readErr atomic.Value
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				var n int
				if err := f.db.R.QueryRow("SELECT n FROM counters WHERE id = 1").Scan(&n); err != nil {
					readErr.Store(err)
				}
			}
		})
	}
	wg.Wait()
	close(stop)
	readers.Wait()
	if failed.Load() != 0 {
		t.Fatalf("%d commands failed", failed.Load())
	}
	if err, _ := readErr.Load().(error); err != nil {
		t.Fatalf("reader: %v", err)
	}
	if n := f.count(t, "SELECT n FROM counters WHERE id = 1"); n != workers*each {
		t.Fatalf("counter %d, want %d", n, workers*each)
	}
	s := f.w.Stats()
	if s.Batches >= workers*each {
		t.Fatalf("no batching: %+v", s)
	}
	t.Logf("%d commands in %d batches (largest %d)", s.Commands, s.Batches, s.MaxBatch)
}

func TestSendAndQueueFull(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{QueueSize: 4})
	release := f.block(t)
	for i := range 4 {
		if err := f.w.Send(increment(1)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if err := f.w.Send(increment(1)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("fifth send: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := f.w.Do(ctx, increment(1)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do on a full queue: %v", err)
	}
	if err := f.w.Send(Func(func(*Tx) error { return errors.New("nobody waits for this") })); !errors.Is(err, ErrQueueFull) {
		t.Fatal(err)
	}
	release()
	if err := f.w.Do(context.Background(), Func(func(tx *Tx) error { return errors.New("sent and failed") })); err == nil {
		t.Fatal("no error")
	}
	if err := f.w.Send(Func(func(tx *Tx) error { return errors.New("sent and failed") })); err != nil {
		t.Fatal(err)
	}
	if err := f.w.Do(context.Background(), Func(func(*Tx) error { return nil })); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT n FROM counters WHERE id = 1"); n != 4 {
		t.Fatalf("counter %d, want the 4 sent", n)
	}
	if log := f.log.String(); strings.Count(log, "sent and failed") != 1 {
		t.Fatalf("want the failed Send logged, and not the failed Do:\n%s", log)
	}
}

type who struct{}

func TestDoReportsTheOutcomeAfterItsContextEnds(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	release := f.block(t)
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), who{}, "anna"))
	var got any
	done := make(chan error)
	go func() {
		done <- f.w.Do(ctx, Func(func(tx *Tx) error {
			got = tx.Context().Value(who{})
			if tx.Context().Err() != nil {
				return errors.New("the command's context was cancelled")
			}
			return increment(1)(tx)
		}))
	}()
	f.queued(t, 1)
	cancel()
	release()
	if err := <-done; err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got != "anna" {
		t.Fatalf("the context's values are lost: %v", got)
	}
	if n := f.count(t, "SELECT n FROM counters WHERE id = 1"); n != 1 {
		t.Fatal("the command did not run")
	}
	if err := f.w.Do(ctx, increment(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Do with a cancelled context: %v", err)
	}
}

type checked struct{ ok bool }

func (c checked) Apply(tx *Tx) error { return increment(1)(tx) }
func (c checked) Validate() error {
	if !c.ok {
		return errors.New("not ok")
	}
	return nil
}

func TestValidate(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := context.Background()
	for _, err := range []error{f.w.Send(checked{}), f.w.Do(ctx, checked{}), f.w.Send(nil), f.w.Do(ctx, nil)} {
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("got %v", err)
		}
	}
	if err := f.w.Do(ctx, checked{}); !strings.Contains(err.Error(), "not ok") {
		t.Fatalf("the validator's error is lost: %v", err)
	}
	if err := f.w.Do(ctx, checked{ok: true}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT n FROM counters WHERE id = 1"); n != 1 {
		t.Fatalf("counter %d, want only the valid command", n)
	}
}

func TestCloseDrainsTheQueue(t *testing.T) {
	db := openDB(t, sqlite.Options{})
	defer db.Close()
	w, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan error, 1)
	go func() { ran <- w.Run(context.Background()) }()
	var wg sync.WaitGroup
	var ok atomic.Int64
	for range 100 {
		wg.Go(func() {
			if w.Do(context.Background(), increment(1)) == nil {
				ok.Add(1)
			}
		})
	}
	time.Sleep(5 * time.Millisecond)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-ran; err != nil {
		t.Fatalf("Run: %v", err)
	}
	wg.Wait()
	if err := w.Do(context.Background(), increment(1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Do after Close: %v", err)
	}
	if err := w.Send(increment(1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Send after Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	var n int
	if err := db.R.QueryRow("SELECT n FROM counters WHERE id = 1").Scan(&n); err != nil || int64(n) != ok.Load() {
		t.Fatalf("counter %d, accepted %d, %v", n, ok.Load(), err)
	}
	// The writer gave its connection back.
	if _, err := db.W.Exec("UPDATE counters SET n = 0"); err != nil {
		t.Fatal(err)
	}
}

func TestRunStopsWithItsContext(t *testing.T) {
	db := openDB(t, sqlite.Options{})
	defer db.Close()
	w, err := New(db, Options{QueueSize: 8})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- w.Run(ctx) }()
	f := &fixture{db: db, w: w}
	release := f.block(t)
	var wg sync.WaitGroup
	for range 20 { // more than the queue holds: some Do wait for room
		wg.Go(func() { w.Do(context.Background(), increment(1)) })
	}
	f.queued(t, 8)
	cancel()
	release()
	if err := <-ran; err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if n := f.count(t, "SELECT n FROM counters WHERE id = 1"); n != 20 {
		t.Fatalf("counter %d: accepted commands were dropped", n)
	}
	if err := w.Send(increment(1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Send after Run stopped: %v", err)
	}
	if err := w.Run(context.Background()); err == nil {
		t.Fatal("Run ran twice")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseWithoutRun(t *testing.T) {
	db := openDB(t, sqlite.Options{})
	defer db.Close()
	w, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := w.Send(increment(2)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.R.QueryRow("SELECT n FROM counters WHERE id = 2").Scan(&n); err != nil || n != 3 {
		t.Fatalf("counter %d, %v", n, err)
	}
}

func TestOneWriterPerDatabase(t *testing.T) {
	db := openDB(t, sqlite.Options{BusyTimeout: 50 * time.Millisecond})
	defer db.Close()
	w, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := New(db, Options{}); err == nil {
		t.Fatal("a second writer took the connection")
	}
}

func TestNewRefusesReadOnly(t *testing.T) {
	db := openDB(t, sqlite.Options{})
	db.Close()
	ro, err := sqlite.Open(context.Background(), db.Path, sqlite.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := New(ro, Options{}); err == nil || !strings.Contains(err.Error(), "ReadOnly") {
		t.Fatalf("a writer on a read-only database: %v", err)
	}
}

func TestOnCommitTopics(t *testing.T) {
	var mu sync.Mutex
	var got [][]string
	f := start(t, sqlite.Options{}, Options{OnCommit: func(topics []string) {
		mu.Lock()
		got = append(got, topics)
		mu.Unlock()
	}})
	published := func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
	release := f.block(t)
	cmds := []Command{
		Func(func(tx *Tx) error { tx.Touch("board:2", "board:1"); tx.Touch("board:2"); return nil }),
		Func(func(tx *Tx) error { tx.Touch("board:3"); return errors.New("refused") }),
		Func(func(tx *Tx) error {
			tx.Touch("board:1")
			tx.Savepoint(func(tx *Tx) error { tx.Touch("board:4"); return errors.New("inner") })
			return nil
		}),
		Func(func(tx *Tx) error { tx.Touch("board:5"); panic("touched, then broke") }),
		Func(func(*Tx) error { return nil }),
	}
	var wg sync.WaitGroup
	for i, cmd := range cmds {
		wg.Go(func() {
			f.w.Do(context.Background(), cmd)
			if i == 0 && len(published()) < 2 {
				t.Error("Do returned before OnCommit had the topics")
			}
		})
		f.queued(t, i+1)
	}
	release()
	wg.Wait()
	want := [][]string{nil, {"board:1", "board:2"}}
	if p := published(); !slices.EqualFunc(p, want, slices.Equal) {
		t.Fatalf("published %q, want %q", p, want)
	}
	if err := f.w.Do(context.Background(), Func(func(tx *Tx) error { tx.Touch("x"); return errors.New("no") })); err == nil {
		t.Fatal("no error")
	}
	if p := published(); len(p) != 3 || p[2] != nil {
		t.Fatalf("a batch of a failed command published %q", p[2:])
	}
}

func TestAfterCommit(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	var mu sync.Mutex
	var ran []string
	note := func(s string) func() {
		return func() { mu.Lock(); ran = append(ran, s); mu.Unlock() }
	}
	release := f.block(t)
	cmds := []Command{
		Func(func(tx *Tx) error { tx.AfterCommit(note("a1")); tx.AfterCommit(note("a2")); return nil }),
		Func(func(tx *Tx) error { tx.AfterCommit(note("failed")); return errors.New("no") }),
		Func(func(tx *Tx) error {
			tx.Savepoint(func(tx *Tx) error { tx.AfterCommit(note("inner")); return errors.New("no") })
			tx.AfterCommit(note("c"))
			return nil
		}),
		Func(func(tx *Tx) error { tx.AfterCommit(func() { panic("after commit") }); return nil }),
		Func(func(tx *Tx) error { tx.AfterCommit(note("e")); return nil }),
	}
	var wg sync.WaitGroup
	for i, cmd := range cmds {
		wg.Go(func() { f.w.Do(context.Background(), cmd) })
		f.queued(t, i+1)
	}
	release()
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(ran, []string{"a1", "a2", "c", "e"}) {
		t.Fatalf("ran %v", ran)
	}
	if !strings.Contains(f.log.String(), "after commit") {
		t.Fatal("the panic in AfterCommit was not logged")
	}
}

func TestSavepointUndoesInnerOnly(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	err := f.w.Do(context.Background(), Func(func(tx *Tx) error {
		if err := insert("outer")(tx); err != nil {
			return err
		}
		inner := tx.Savepoint(func(tx *Tx) error {
			if err := insert("inner")(tx); err != nil {
				return err
			}
			return errors.New("rejected")
		})
		if inner == nil || inner.Error() != "rejected" {
			t.Errorf("inner error: %v", inner)
		}
		panicked := tx.Savepoint(func(tx *Tx) error {
			insert("panicked")(tx)
			panic("inner panic")
		})
		if !errors.Is(panicked, ErrPanic) {
			t.Errorf("inner panic: %v", panicked)
		}
		return tx.Savepoint(insert("kept"))
	}))
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT count(*) FROM items WHERE name IN ('outer', 'kept')"); n != 2 {
		t.Fatal("outer inserts lost")
	}
	if n := f.count(t, "SELECT count(*) FROM items WHERE name IN ('inner', 'panicked')"); n != 0 {
		t.Fatal("rolled-back inserts kept")
	}
}

// Running the same command again reuses its prepared statements; a
// statement with an error fails its command only.
func TestStatementsPreparedOnce(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := context.Background()
	rename := func(name string) error {
		return f.w.Do(ctx, Func(func(tx *Tx) error {
			_, err := tx.Exec("UPDATE items SET name = ? WHERE id = ?", name, 1)
			return err
		}))
	}
	cached := func() (n int) {
		f.w.Do(ctx, Func(func(*Tx) error { n = len(f.w.stmts); return nil }))
		return n
	}
	if err := f.w.Do(ctx, insert("x")); err != nil {
		t.Fatal(err)
	}
	if err := rename("y"); err != nil {
		t.Fatal(err)
	}
	before := cached()
	for i := range 20 {
		if err := rename(fmt.Sprint("y", i)); err != nil {
			t.Fatal(err)
		}
	}
	if after := cached(); after != before {
		t.Fatalf("%d statements cached after one run, %d after 21", before, after)
	}
	err := f.w.Do(ctx, Func(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE no_such_table SET x = 1")
		return err
	}))
	if err == nil || !strings.Contains(err.Error(), "no_such_table") {
		t.Fatalf("a statement on a missing table: %v", err)
	}
	if err := rename("ok"); err != nil {
		t.Fatalf("the writer broke after a bad statement: %v", err)
	}
	if n := f.count(t, "SELECT count(*) FROM items WHERE name = 'ok'"); n != 1 {
		t.Fatalf("rename after the bad statement: %d rows", n)
	}
}

// A query that runs again while its rows are open gets a statement of its
// own, and rows a command leaves open are closed when it returns.
func TestNestedQueries(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := context.Background()
	for _, name := range []string{"a", "b", "c"} {
		if err := f.w.Do(ctx, insert(name)); err != nil {
			t.Fatal(err)
		}
	}
	names := func(tx *Tx) ([]string, error) {
		rows, err := tx.Query("SELECT name FROM items ORDER BY name")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return nil, err
			}
			out = append(out, n)
		}
		return out, rows.Err()
	}
	var pairs []string
	err := f.w.Do(ctx, Func(func(tx *Tx) error {
		rows, err := tx.Query("SELECT name FROM items ORDER BY name")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var outer string
			if err := rows.Scan(&outer); err != nil {
				return err
			}
			inner, err := names(tx)
			if err != nil {
				return err
			}
			for _, n := range inner {
				pairs = append(pairs, outer+n)
			}
		}
		return rows.Err()
	}))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"aa", "ab", "ac", "ba", "bb", "bc", "ca", "cb", "cc"}; !slices.Equal(pairs, want) {
		t.Fatalf("pairs %v", pairs)
	}

	// A command that leaves its rows open does not break the next one.
	err = f.w.Do(ctx, Func(func(tx *Tx) error {
		rows, err := tx.Query("SELECT name FROM items ORDER BY name")
		if err == nil {
			rows.Next()
		}
		return err
	}))
	if err != nil {
		t.Fatal(err)
	}
	var again []string
	err = f.w.Do(ctx, Func(func(tx *Tx) error {
		var err error
		again, err = names(tx)
		return err
	}))
	if err != nil || len(again) != 3 {
		t.Fatalf("after leaked rows: %v, %v", again, err)
	}
}

// itemCount is query code written once for every Querier: the read pool,
// a read transaction and a command's Tx.
func itemCount(ctx context.Context, q sqlite.Querier, name string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, "SELECT count(*) FROM items WHERE name = ?", name).Scan(&n)
	return n, err
}

// A command's Tx serves query code that takes a sqlite.Querier: it runs on
// the batch's transaction with its statement cache, and a cancelled ctx
// does not stop it halfway through a batch.
func TestTxIsAQuerier(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	add := func(name string) error {
		return f.w.Do(ctx, Func(func(tx *Tx) error {
			if _, err := tx.ExecContext(cancelled, "INSERT INTO items (name) VALUES (?)", name); err != nil {
				return err
			}
			n, err := itemCount(cancelled, tx, name)
			if err == nil && n != 1 {
				err = fmt.Errorf("the command sees %d rows of its own insert", n)
			}
			return err
		}))
	}
	if err := add("a"); err != nil {
		t.Fatal(err)
	}
	var before int
	f.w.Do(ctx, Func(func(*Tx) error { before = len(f.w.stmts); return nil }))
	for i := range 5 {
		if err := add(fmt.Sprint("b", i)); err != nil {
			t.Fatal(err)
		}
	}
	err := f.w.Do(ctx, Func(func(tx *Tx) error {
		if len(f.w.stmts) != before {
			t.Errorf("%d statements cached after one run, %d after six", before, len(f.w.stmts))
		}
		rows, err := tx.QueryContext(cancelled, "SELECT name FROM items ORDER BY name")
		if err != nil {
			return err
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			names = append(names, n)
		}
		if len(names) != 6 {
			t.Errorf("read %v", names)
		}
		return rows.Err()
	}))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := itemCount(ctx, f.db.R, "a"); err != nil || n != 1 {
		t.Fatalf("on the read pool: %d, %v", n, err)
	}
	err = f.db.View(ctx, func(tx *sql.Tx) error {
		n, err := itemCount(ctx, tx, "b4")
		if err == nil && n != 1 {
			err = fmt.Errorf("%d rows", n)
		}
		return err
	})
	if err != nil {
		t.Fatalf("in a read transaction: %v", err)
	}
}

func TestTxAfterItsCommand(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	var kept *Tx
	if err := f.w.Do(context.Background(), Func(func(tx *Tx) error { kept = tx; return nil })); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("using a Tx after its command did not panic")
		}
	}()
	kept.Exec("DELETE FROM items")
}

func TestStats(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := context.Background()
	for i := range 10 {
		f.w.Do(ctx, increment(1))
		if i%2 == 0 {
			f.w.Do(ctx, Func(func(*Tx) error { return errors.New("no") }))
		}
	}
	release := f.block(t)
	var wg sync.WaitGroup
	for i := range 5 {
		wg.Go(func() { f.w.Do(ctx, increment(2)) })
		f.queued(t, i+1)
	}
	release()
	wg.Wait()
	s := f.w.Stats()
	if s.Commands != 21 || s.Failures != 5 || s.Batches != 17 || s.MaxBatch != 5 || s.Queue != 0 {
		t.Fatalf("%+v", s)
	}
	if s.LastCommit <= 0 || s.P99Commit <= 0 || s.Busy < s.P99Commit {
		t.Fatalf("%+v", s)
	}
}

func TestReadsDuringWritesSeeCommittedBatches(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := context.Background()
	err := f.db.View(ctx, func(tx *sql.Tx) error {
		var before int
		if err := tx.QueryRow("SELECT n FROM counters WHERE id = 1").Scan(&before); err != nil {
			return err
		}
		if err := f.w.Do(ctx, increment(1)); err != nil {
			return err
		}
		var after int
		if err := tx.QueryRow("SELECT n FROM counters WHERE id = 1").Scan(&after); err != nil {
			return err
		}
		if after != before {
			t.Errorf("a read transaction saw a later commit: %d, then %d", before, after)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT n FROM counters WHERE id = 1"); n != 1 {
		t.Fatalf("after the view: %d", n)
	}
}
