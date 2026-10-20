package writerrace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing/fstest"
	"time"

	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"
)

// raceSchema is the race file's: lith's writer benchmark, accounts of ten
// for each producer and a ledger.
var raceSchema = fstest.MapFS{"001_bench.sql": {Data: []byte(`
CREATE TABLE accounts (id INTEGER PRIMARY KEY, balance INTEGER NOT NULL, version INTEGER NOT NULL DEFAULT 0) STRICT;
CREATE TABLE ledger (
	id      INTEGER PRIMARY KEY,
	from_id INTEGER NOT NULL REFERENCES accounts (id),
	to_id   INTEGER NOT NULL REFERENCES accounts (id),
	amount  INTEGER NOT NULL,
	at      INTEGER NOT NULL
) STRICT;
`)}}

var errInsufficient = errors.New("insufficient funds")

// transfer is one producer's command, the one lith's writer benchmark
// sends: read a balance, debit one account, credit another, append to the
// ledger.
type transfer struct {
	from, to, amount int64
	topic            string
}

func (c transfer) Apply(tx *writer.Tx) error {
	var balance int64
	if err := tx.QueryRow(`SELECT balance FROM accounts WHERE id = ?`, c.from).Scan(&balance); err != nil {
		return err
	}
	if balance < c.amount {
		return errInsufficient
	}
	debit := `UPDATE accounts SET balance = balance - ?, version = version + 1 WHERE id = ?`
	if _, err := tx.Exec(debit, c.amount, c.from); err != nil {
		return err
	}
	credit := `UPDATE accounts SET balance = balance + ?, version = version + 1 WHERE id = ?`
	if _, err := tx.Exec(credit, c.amount, c.to); err != nil {
		return err
	}
	entry := `INSERT INTO ledger (from_id, to_id, amount, at) VALUES (?, ?, ?, ?)`
	if _, err := tx.Exec(entry, c.from, c.to, c.amount, time.Now().Unix()); err != nil {
		return err
	}
	tx.Touch(c.topic)
	return nil
}

// produced is a producer's transfer: once it has committed, it hands the
// producer back to the race's driver, which sends the producer's next.
type produced struct {
	transfer
	producer int64
	done     chan<- int64
}

func (c produced) Apply(tx *writer.Tx) error {
	if err := c.transfer.Apply(tx); err != nil {
		return err
	}
	tx.AfterCommit(func() { c.done <- c.producer })
	return nil
}

// Sample is what the race's writer reports at one moment, from
// writer.Stats, counted from the race's start.
type Sample struct {
	At       time.Duration // since the producers started
	Commands int64         // committed or failed so far
	Batches  int64         // transactions so far
	Failures int64
	Queue    int           // commands waiting now
	MaxBatch int           // the largest transaction so far
	P99      time.Duration // of the last 1024 transactions
	Sizes    Sizes         // transactions by their commands
}

// Sizes counts transactions by the commands they took, in powers of two:
// 1, 2 to 3, 4 to 7 and so on; the last bucket, 128 and more, holds every
// larger one (a race has 200 producers at most, so at most 200 commands
// wait for one transaction).
type Sizes [8]int64

func sizesOf(s [writer.SizeBuckets]int64) Sizes {
	var out Sizes
	for i, n := range s {
		out[min(i, len(out)-1)] += n
	}
	return out
}

func (s Sizes) minus(o Sizes) Sizes {
	for i := range s {
		s[i] -= o[i]
	}
	return s
}

// Engine runs races: a writer of its own on a file of its own, opened for
// one race and removed after it.
type Engine struct {
	// Dir holds the race's file while it runs.
	Dir string
	// Duration is how long the producers send.
	Duration time.Duration
	// Every is the time between two samples.
	Every time.Duration
	Log   *slog.Logger
}

// path is the race's file.
func (e *Engine) path(id int64) string {
	return filepath.Join(e.Dir, fmt.Sprintf("writer-race-%d.db", id))
}

// Clean removes the files of races that a process left behind when it
// stopped in the middle of one.
func (e *Engine) Clean() {
	files, _ := filepath.Glob(filepath.Join(e.Dir, "writer-race-*.db*"))
	for _, f := range files {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			e.Log.Warn("writer race: remove a file left behind", "file", f, "err", err)
		}
	}
}

// Run runs race id: it opens the race's file, seeds ten accounts for each
// producer, and lets the producers send transfers for Duration, each
// waiting for its commit before it sends the next, as a request handler
// waits in Do. One goroutine, the driver, sends for all of them, on a
// thread with the writer's lower priority, and a commit hands its producer
// back to it: so the producers cost no goroutine at the site's priority.
// It calls sample every Every with the writer's numbers, and once more at
// the end. When ctx ends, the producers stop early. Whatever happens, the
// file is gone when Run returns.
func (e *Engine) Run(ctx context.Context, id int64, s Settings, sample func(Sample)) (Sample, error) {
	path := e.path(id)
	defer e.remove(path)
	db, err := sqlite.Open(ctx, path, sqlite.Options{
		Synchronous: s.Sync,
		Migrations:  raceSchema,
		Checkpoint:  sqlite.CheckpointAuto, // as in lith's writer benchmark
		Readers:     1,                     // nobody reads the race's file
		CacheKiB:    2048,
		MmapBytes:   -1,
	})
	if err != nil {
		return Sample{}, err
	}
	defer db.Close()
	// A producer's commit hands it back to the driver (produced), a
	// failure ends it.
	committed := make(chan int64, s.Producers)
	failures := make(chan error, s.Producers)
	w, err := writer.New(db, writer.Options{
		QueueSize:     256, // a producer has one command in flight at most
		OptimizeEvery: -1,
		Log:           e.Log,
		OnSendError:   func(_ writer.Command, err error) { failures <- err },
		// Nothing renders from the race's file, but the writer collects
		// and sorts each batch's topics for OnCommit as an app's does for
		// its hub, and as lith's benchmark has it do.
		OnCommit: func([]string) {},
	})
	if err != nil {
		return Sample{}, err
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		if runtime.GOMAXPROCS(0) > 1 {
			// The thread is never unlocked: it ends with this goroutine,
			// its lowered priority with it.
			runtime.LockOSThread()
			if err := lowerPriority(); err != nil {
				e.Log.Debug("writer race: lower the writer's priority", "err", err)
			}
		}
		w.Run(context.Background())
	}()
	defer func() {
		w.Close()
		<-stopped
	}()

	err = w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
		for id := range int64(s.Producers * Accounts) {
			if _, err := tx.Exec(`INSERT INTO accounts (id, balance) VALUES (?, ?)`, id+1, int64(1e12)); err != nil {
				return err
			}
		}
		return nil
	}))
	if err != nil {
		return Sample{}, err
	}

	base := w.Stats()
	start := time.Now()
	read := func() Sample {
		st := w.Stats()
		return Sample{
			At:       time.Since(start),
			Commands: st.Commands - base.Commands,
			Batches:  st.Batches - base.Batches,
			Failures: st.Failures - base.Failures,
			Queue:    st.Queue,
			MaxBatch: st.MaxBatch,
			P99:      st.P99Commit,
			Sizes:    sizesOf(st.Sizes).minus(sizesOf(base.Sizes)),
		}
	}
	deadline := start.Add(e.Duration)
	topics := make([]string, s.Producers)
	sent := make([]int64, s.Producers) // each producer's transfers so far
	for p := range topics {
		topics[p] = fmt.Sprint("producer:", p)
	}
	send := func(p int64) error {
		first, i := p*Accounts+1, sent[p]
		sent[p]++
		c := transfer{from: first + i%Accounts, to: first + (i+1)%Accounts, amount: 1 + i%100, topic: topics[p]}
		return w.Send(produced{transfer: c, producer: p, done: committed})
	}
	var failed error
	done := make(chan struct{})
	go func() {
		defer close(done)
		if runtime.GOMAXPROCS(0) > 1 {
			runtime.LockOSThread() // never unlocked, as the writer's thread
			if err := lowerPriority(); err != nil {
				e.Log.Debug("writer race: lower the producers' priority", "err", err)
			}
		}
		fail := func(err error) {
			if failed == nil && ctx.Err() == nil {
				failed = err
			}
		}
		waiting := 0 // producers whose transfer has not committed yet
		for p := range int64(s.Producers) {
			if err := send(p); err != nil {
				fail(err)
				continue
			}
			waiting++
		}
		for waiting > 0 {
			select {
			case p := <-committed:
				waiting--
				if time.Now().Before(deadline) && ctx.Err() == nil {
					if err := send(p); err != nil {
						fail(err)
						continue
					}
					waiting++
				}
			case err := <-failures:
				waiting-- // that producer stops
				fail(err)
			}
		}
	}()
	t := time.NewTicker(e.Every)
	defer t.Stop()
	for running := true; running; {
		select {
		case <-t.C:
			sample(read())
		case <-done:
			running = false
		}
	}
	last := read()
	last.Queue = 0
	sample(last)
	if failed == nil {
		failed = ctx.Err()
	}
	return last, failed
}

func (e *Engine) remove(path string) {
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			e.Log.Warn("writer race: remove the race's file", "file", f, "err", err)
		}
	}
}
