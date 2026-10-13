package writer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/dbmetrics"
	"github.com/zweiundeins/lith/sqlite"
)

func walSize(t *testing.T, db *sqlite.DB) int64 {
	t.Helper()
	st, err := os.Stat(db.Path + "-wal")
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

// fill commits batches of about 16 pages each.
func fill(t *testing.T, f *fixture, batches int) {
	t.Helper()
	blob := make([]byte, 60000)
	for range batches {
		err := f.w.Do(context.Background(), Func(func(tx *Tx) error {
			_, err := tx.Exec("INSERT INTO blobs (data) VALUES (?)", blob)
			return err
		}))
		if err != nil {
			t.Fatal(err)
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 5s for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCheckpointWriterKeepsTheWALSmall(t *testing.T) {
	t.Parallel()
	f := start(t, sqlite.Options{Checkpoint: sqlite.CheckpointWriter}, Options{CheckpointPages: 64})
	fill(t, f, 200) // about 3200 pages, 13 MB of WAL without checkpoints
	if size := walSize(t, f.db); size > 1<<20 {
		t.Fatalf("WAL %d bytes after 200 batches", size)
	}
	waitFor(t, "the idle checkpoint to truncate the WAL", func() bool { return walSize(t, f.db) == 0 })
	if n := f.count(t, "SELECT count(*) FROM blobs"); n != 200 {
		t.Fatalf("%d rows", n)
	}
}

func TestCheckpointExternalLeavesTheWAL(t *testing.T) {
	t.Parallel()
	f := start(t, sqlite.Options{Checkpoint: sqlite.CheckpointExternal}, Options{CheckpointPages: 64})
	fill(t, f, 200)
	size := walSize(t, f.db)
	if size < 10<<20 {
		t.Fatalf("WAL %d bytes: something checkpointed it", size)
	}
	time.Sleep(1500 * time.Millisecond)
	if after := walSize(t, f.db); after != size {
		t.Fatalf("WAL went from %d to %d bytes while idle", size, after)
	}
}

// A reader holding an old snapshot keeps the WAL from being truncated, but
// the writer does not wait for it.
func TestIdleCheckpointDoesNotWaitForReaders(t *testing.T) {
	t.Parallel()
	f := start(t, sqlite.Options{Checkpoint: sqlite.CheckpointWriter}, Options{CheckpointPages: 64})
	ctx := context.Background()
	fill(t, f, 4)
	held, hold, viewed := make(chan struct{}), make(chan struct{}), make(chan error)
	go func() {
		viewed <- f.db.View(ctx, func(tx *sql.Tx) error {
			var n int
			err := tx.QueryRow("SELECT count(*) FROM blobs").Scan(&n)
			close(held)
			<-hold
			return err
		})
	}()
	<-held
	fill(t, f, 4)
	time.Sleep(1500 * time.Millisecond) // the idle checkpoint runs, and finds the reader
	start := time.Now()
	if err := f.w.Do(ctx, increment(1)); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("a command waited %v behind the checkpoint", d)
	}
	if walSize(t, f.db) == 0 {
		t.Fatal("the WAL was truncated under a reader")
	}
	close(hold)
	if err := <-viewed; err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the WAL to be truncated after the reader left", func() bool { return walSize(t, f.db) == 0 })
}

// registrations keeps driver names unique when a test runs more than once
// in one process: sql.Register cannot be undone.
var registrations atomic.Int32

// A driver that wraps modernc's (sqlite.Options.Driver) still lets the
// writer read what it wrote to the WAL, so it checkpoints.
func TestCheckpointThroughAWrappingDriver(t *testing.T) {
	t.Parallel()
	name := fmt.Sprintf("sqlite-wrapped-%d", registrations.Add(1))
	if err := dbmetrics.Register(name, "sqlite", dbmetrics.Options{SlowThreshold: -1}); err != nil {
		t.Fatal(err)
	}
	f := start(t, sqlite.Options{Checkpoint: sqlite.CheckpointWriter, Driver: name}, Options{CheckpointPages: 64})
	fill(t, f, 200)
	if size := walSize(t, f.db); size > 1<<20 {
		t.Fatalf("WAL %d bytes after 200 batches", size)
	}
	if log := f.log.String(); strings.Contains(log, "WAL size") {
		t.Fatalf("the writer could not read the WAL size:\n%s", log)
	}
}
