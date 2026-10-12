package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func open(t *testing.T, opts Options) *DB {
	t.Helper()
	if opts.Synchronous == "" {
		opts.Synchronous = "NORMAL"
	}
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func pragma(t *testing.T, c interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, name string) string {
	t.Helper()
	var v string
	if err := c.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

func TestSynchronousIsRequired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	for _, s := range []string{"", "OFF", "EXTRA"} {
		if _, err := Open(context.Background(), path, Options{Synchronous: s}); err == nil || !strings.Contains(err.Error(), "Synchronous") {
			t.Errorf("Synchronous %q: %v", s, err)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused Open created the file: %v", err)
	}
	d := open(t, Options{Synchronous: "normal"})
	if got := pragma(t, d.W, "synchronous"); got != "1" {
		t.Errorf("synchronous %s, want 1 (NORMAL)", got)
	}
}

func TestPragmas(t *testing.T) {
	ctx := context.Background()
	d := open(t, Options{Synchronous: "FULL", Checkpoint: CheckpointWriter, Readers: 3})
	common := map[string]string{
		"journal_mode": "wal",
		"synchronous":  "2",
		"foreign_keys": "1",
		"busy_timeout": "5000",
		"temp_store":   "2",
		"cache_size":   "-32000",
	}
	for name, want := range common {
		if got := pragma(t, d.W, name); got != want {
			t.Errorf("writer: %s = %s, want %s", name, got, want)
		}
	}
	if got := pragma(t, d.W, "wal_autocheckpoint"); got != "0" {
		t.Errorf("writer: wal_autocheckpoint = %s, want 0", got)
	}
	if got := pragma(t, d.W, "query_only"); got != "0" {
		t.Errorf("writer: query_only = %s", got)
	}
	// Check every connection of the read pool, not just the first.
	conns := make([]*sql.Conn, 3)
	for i := range conns {
		c, err := d.R.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		conns[i] = c
		for name, want := range common {
			if got := pragma(t, c, name); got != want {
				t.Errorf("reader %d: %s = %s, want %s", i, name, got, want)
			}
		}
		if got := pragma(t, c, "query_only"); got != "1" {
			t.Errorf("reader %d: query_only = %s", i, got)
		}
		if got := pragma(t, c, "mmap_size"); got != "268435456" {
			t.Errorf("reader %d: mmap_size = %s", i, got)
		}
	}

	auto := open(t, Options{})
	if got := pragma(t, auto.W, "wal_autocheckpoint"); got != "1000" {
		t.Errorf("CheckpointAuto: wal_autocheckpoint = %s, want SQLite's 1000", got)
	}
	if got := pragma(t, auto.W, "synchronous"); got != "1" {
		t.Errorf("synchronous = %s, want 1", got)
	}
}

func TestReadPoolIsQueryOnlyAndKeepsItsConnections(t *testing.T) {
	ctx := context.Background()
	d := open(t, Options{Readers: 4})
	if _, err := d.W.Exec("CREATE TABLE t (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	_, err := d.R.Exec("INSERT INTO t VALUES (1)")
	if err == nil || !strings.Contains(err.Error(), "readonly") {
		t.Fatalf("insert through R: %v", err)
	}
	err = d.View(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO t VALUES (1)")
		return err
	})
	if err == nil {
		t.Fatal("insert in View succeeded")
	}

	// Busy readers, then quiet: the pool keeps every connection open.
	for round := range 3 {
		var wg sync.WaitGroup
		hold, got := make(chan struct{}), make(chan struct{})
		for range 4 {
			wg.Go(func() {
				c, err := d.R.Conn(ctx)
				if err != nil {
					t.Error(err)
					close(got)
					return
				}
				got <- struct{}{}
				<-hold
				c.Close()
			})
		}
		for range 4 {
			<-got
		}
		close(hold)
		wg.Wait()
		s := d.R.Stats()
		if s.Idle != 4 || s.MaxIdleClosed != 0 || s.MaxLifetimeClosed != 0 || s.MaxIdleTimeClosed != 0 {
			t.Fatalf("round %d: %+v", round, s)
		}
	}
}

func TestViewSeesOneSnapshot(t *testing.T) {
	ctx := context.Background()
	d := open(t, Options{})
	if _, err := d.W.Exec("CREATE TABLE t (x INTEGER); INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	count := func(q interface {
		QueryRow(string, ...any) *sql.Row
	}) int {
		var n int
		if err := q.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	err := d.View(ctx, func(tx *sql.Tx) error {
		before := count(tx)
		if _, err := d.W.Exec("INSERT INTO t VALUES (2)"); err != nil {
			return err
		}
		if after := count(tx); after != before {
			t.Errorf("the snapshot moved: %d, then %d", before, after)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := count(d.R); n != 2 {
		t.Fatalf("after View: %d rows", n)
	}
}

func TestBackup(t *testing.T) {
	ctx := context.Background()
	d := open(t, Options{})
	if _, err := d.W.Exec("CREATE TABLE t (x INTEGER); INSERT INTO t VALUES (1), (2), (3)"); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "copy.db")
	if err := d.Backup(ctx, dst); err != nil {
		t.Fatal(err)
	}
	c, err := Open(ctx, dst, Options{Synchronous: "NORMAL"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var n int
	if err := c.R.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil || n != 3 {
		t.Fatalf("copy: %d rows, %v", n, err)
	}

	if err := d.Backup(ctx, dst); err == nil {
		t.Fatal("Backup overwrote a file")
	}
	if err := c.R.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil || n != 3 {
		t.Fatalf("the refused backup changed the file: %d rows, %v", n, err)
	}
	bad := filepath.Join(t.TempDir(), "bad.db")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := d.Backup(cancelled, bad); err == nil {
		t.Fatal("a cancelled backup succeeded")
	}
	if _, err := os.Stat(bad); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed backup left its file: %v", err)
	}
}

func TestPathIsEscaped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a?b#c%41.db")
	d, err := Open(context.Background(), path, Options{Synchronous: "NORMAL"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec("CREATE TABLE t (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryDatabaseRefused(t *testing.T) {
	if _, err := Open(context.Background(), ":memory:", Options{Synchronous: "NORMAL"}); err == nil {
		t.Fatal("opened an in-memory database: the pools would not share it")
	}
}
