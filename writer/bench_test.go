package writer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/zweiundeins/lith/sqlite"
)

const benchSchema = `
CREATE TABLE accounts (id INTEGER PRIMARY KEY, balance INTEGER NOT NULL, version INTEGER NOT NULL DEFAULT 0) STRICT;
CREATE TABLE ledger (
	id      INTEGER PRIMARY KEY,
	from_id INTEGER NOT NULL REFERENCES accounts (id),
	to_id   INTEGER NOT NULL REFERENCES accounts (id),
	amount  INTEGER NOT NULL,
	at      INTEGER NOT NULL
) STRICT;
`

var errInsufficient = errors.New("insufficient funds")

// transfer is a command of four statements, like a request that checks
// what it changes: read a balance, debit, credit, append to the ledger.
type transfer struct {
	from, to, amount int64
	topic            string
}

func (c transfer) Apply(tx *Tx) error {
	var balance int64
	if err := tx.QueryRow("SELECT balance FROM accounts WHERE id = ?", c.from).Scan(&balance); err != nil {
		return err
	}
	if balance < c.amount {
		return errInsufficient
	}
	if _, err := tx.Exec("UPDATE accounts SET balance = balance - ?, version = version + 1 WHERE id = ?", c.amount, c.from); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE accounts SET balance = balance + ?, version = version + 1 WHERE id = ?", c.amount, c.to); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO ledger (from_id, to_id, amount, at) VALUES (?, ?, ?, ?)", c.from, c.to, c.amount, time.Now().Unix()); err != nil {
		return err
	}
	tx.Touch(c.topic)
	return nil
}

// BenchmarkWriter is the writer alone, without HTTP: 1, 50 or 200
// producers, each moving money between ten accounts of its own and
// waiting for the commit (Do), as a request handler does. With FULL every
// commit waits for an fsync; many producers make bigger batches.
//
//	go test ./writer -run '^$' -bench 'Writer$' -benchtime 5s
func BenchmarkWriter(b *testing.B) {
	for _, sync := range []string{"FULL", "NORMAL"} {
		for _, producers := range []int{1, 50, 200} {
			b.Run(fmt.Sprintf("%s/producers=%d", sync, producers), func(b *testing.B) {
				benchTransfers(b, sync, sqlite.CheckpointAuto, producers)
			})
		}
	}
}

// BenchmarkWriterCheckpoint is the busiest case of BenchmarkWriter with
// sqlite.CheckpointWriter: the writer checkpoints after answering a batch,
// where SQLite would checkpoint inside the COMMIT that passes 1000 pages.
func BenchmarkWriterCheckpoint(b *testing.B) {
	for _, sync := range []string{"FULL", "NORMAL"} {
		b.Run(sync+"/producers=200", func(b *testing.B) {
			benchTransfers(b, sync, sqlite.CheckpointWriter, 200)
		})
	}
}

func benchTransfers(b *testing.B, synchronous string, checkpoint sqlite.Checkpoint, producers int) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(b.TempDir(), "bench.db"), sqlite.Options{
		Synchronous: synchronous,
		Checkpoint:  checkpoint,
		Migrations:  fstest.MapFS{"001_bench.sql": {Data: []byte(benchSchema)}},
	})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	var topics atomic.Int64
	w, err := New(db, Options{OnCommit: func(t []string) { topics.Add(int64(len(t))) }})
	if err != nil {
		b.Fatal(err)
	}
	go w.Run(ctx)
	defer w.Close()

	const accounts = 10
	err = w.Do(ctx, Func(func(tx *Tx) error {
		for id := range producers * accounts {
			if _, err := tx.Exec("INSERT INTO accounts (id, balance) VALUES (?, ?)", id+1, int64(1e12)); err != nil {
				return err
			}
		}
		return nil
	}))
	if err != nil {
		b.Fatal(err)
	}

	var n atomic.Int64
	s0 := w.Stats()
	b.ResetTimer()
	var wg sync.WaitGroup
	for p := range producers {
		wg.Go(func() {
			first := int64(p*accounts + 1)
			topic := fmt.Sprint("producer:", p)
			for i := n.Add(1); i <= int64(b.N); i = n.Add(1) {
				c := transfer{from: first + i%accounts, to: first + (i+1)%accounts, amount: 1 + i%100, topic: topic}
				if err := w.Do(ctx, c); err != nil {
					b.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	b.StopTimer()
	s := w.Stats()
	batches := float64(s.Batches - s0.Batches)
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "cmds/s")
	b.ReportMetric(float64(s.Commands-s0.Commands)/batches, "cmds/tx")
	b.ReportMetric(float64(s.Busy-s0.Busy)/batches/1e6, "ms/tx")
	b.ReportMetric(float64(s.P99Commit)/1e6, "p99-ms/tx")
	if s.Failures != s0.Failures {
		b.Errorf("%d commands failed", s.Failures-s0.Failures)
	}
}
