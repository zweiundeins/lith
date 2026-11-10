package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/dbmetrics"
	msqlite "modernc.org/sqlite"
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

func TestSynchronous(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	for _, s := range []string{"EXTRA", "0"} {
		if _, err := Open(context.Background(), path, Options{Synchronous: s}); err == nil || !strings.Contains(err.Error(), "Synchronous") {
			t.Errorf("Synchronous %q: %v", s, err)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused Open created the file: %v", err)
	}
	for s, want := range map[string]string{"": "1", "normal": "1", "FULL": "2", "off": "0"} {
		d, err := Open(context.Background(), filepath.Join(t.TempDir(), "s.db"), Options{Synchronous: s})
		if err != nil {
			t.Fatalf("Synchronous %q: %v", s, err)
		}
		if got := pragma(t, d.W, "synchronous"); got != want {
			t.Errorf("Synchronous %q: synchronous is %s, want %s", s, got, want)
		}
		d.Close()
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
		"mmap_size":    "268435456",
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
	}

	auto := open(t, Options{})
	if got := pragma(t, auto.W, "wal_autocheckpoint"); got != "1000" {
		t.Errorf("CheckpointAuto: wal_autocheckpoint = %s, want SQLite's 1000", got)
	}
	if got := pragma(t, auto.W, "synchronous"); got != "1" {
		t.Errorf("synchronous = %s, want 1", got)
	}

	// The write connection maps the file like the readers, in every
	// checkpoint mode and through a wrapping driver; a negative MmapBytes
	// maps nothing on either.
	wrapped := fmt.Sprintf("sqlite-pragmas-%d", registrations.Add(1))
	if err := dbmetrics.Register(wrapped, "sqlite", dbmetrics.Options{SlowThreshold: -1}); err != nil {
		t.Fatal(err)
	}
	for _, o := range []Options{
		{Checkpoint: CheckpointAuto},
		{Checkpoint: CheckpointExternal},
		{Checkpoint: CheckpointExternal, Driver: wrapped, MmapBytes: 64 << 20},
		{Checkpoint: CheckpointWriter, MmapBytes: -1},
	} {
		d := open(t, o)
		want := map[int64]string{0: "268435456", 64 << 20: "67108864", -1: "0"}[o.MmapBytes]
		for name, db := range map[string]*sql.DB{"writer": d.W, "reader": d.R} {
			if got := pragma(t, db, "mmap_size"); got != want {
				t.Errorf("Checkpoint %d, Driver %q, MmapBytes %d: the %s's mmap_size = %s, want %s", o.Checkpoint, o.Driver, o.MmapBytes, name, got, want)
			}
		}
	}
}

// The write connection reads through the map like the readers, in every
// checkpoint mode and through a wrapping driver: a write transaction's read
// of a whole table, as a migration or a command's query makes one, leaves
// its page cache with page 1 and the pages it writes. Without the map the
// read fills the cache to CacheKiB, which stays full while the database is
// open. The transaction's reads see its own writes, which commit and read
// back after the writer checkpoints them into the file it maps.
func TestWriterReadsThroughTheMap(t *testing.T) {
	ctx := context.Background()
	wrapped := fmt.Sprintf("sqlite-writer-map-%d", registrations.Add(1))
	if err := dbmetrics.Register(wrapped, "sqlite", dbmetrics.Options{SlowThreshold: -1}); err != nil {
		t.Fatal(err)
	}
	const cacheKiB = 1000
	for _, tc := range []struct {
		name   string
		opts   Options
		mapped bool
	}{
		{"auto", Options{Checkpoint: CheckpointAuto}, true},
		{"writer", Options{Checkpoint: CheckpointWriter}, true},
		{"external", Options{Checkpoint: CheckpointExternal}, true},
		{"external through a wrapping driver", Options{Checkpoint: CheckpointExternal, Driver: wrapped}, true},
		{"no map", Options{Checkpoint: CheckpointExternal, MmapBytes: -1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.Synchronous, opts.Readers, opts.CacheKiB = "OFF", 1, cacheKiB
			path := filepath.Join(t.TempDir(), "big.db")
			d, err := Open(ctx, path, opts)
			if err != nil {
				t.Fatal(err)
			}
			// About 4.5 MB, over four times the cache, all of it in the file.
			if _, err := d.W.Exec(`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT);
				WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
				INSERT INTO notes (body) SELECT printf('%.200c', 'a') FROM n;
				PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
				t.Fatal(err)
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}

			// Opened again, the write connection starts with an empty cache.
			d, err = Open(ctx, path, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			c, err := d.W.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			tx, err := c.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			var rows, size, n int
			if err := tx.QueryRow(`SELECT count(*), sum(length(body)) FROM notes`).Scan(&rows, &size); err != nil || rows != 20000 {
				t.Fatalf("read %d rows: %v", rows, err)
			}
			if _, err := tx.Exec(`UPDATE notes SET body = 'changed' WHERE id = 10000;
				INSERT INTO notes (body) VALUES ('added')`); err != nil {
				t.Fatal(err)
			}
			if err := tx.QueryRow(`SELECT count(*) FROM notes WHERE body IN ('changed', 'added')`).Scan(&n); err != nil || n != 2 {
				t.Fatalf("the transaction read %d of its 2 writes back: %v", n, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			cache := cacheUsed(t, c)
			if err := d.R.QueryRow(`SELECT count(*) FROM notes WHERE body IN ('changed', 'added')`).Scan(&n); err != nil || n != 2 {
				t.Fatalf("a reader found %d of the 2 writes: %v", n, err)
			}
			var check string
			if _, err := c.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
				t.Fatal(err)
			}
			if err := c.QueryRowContext(ctx, `SELECT count(*) FROM notes WHERE body IN ('changed', 'added')`).Scan(&n); err != nil || n != 2 {
				t.Fatalf("after the checkpoint the writer found %d of the 2 writes: %v", n, err)
			}
			if err := c.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
				t.Fatalf("integrity_check: %s, %v", check, err)
			}
			t.Logf("after reading %d KiB in a write transaction, the writer's page cache holds %d KiB", size>>10, cache>>10)
			if tc.mapped && cache > cacheKiB<<10/10 {
				t.Errorf("the writer's page cache holds %d KiB, want at most a tenth of its %d KiB", cache>>10, cacheKiB)
			}
			if !tc.mapped && cache < cacheKiB<<10*9/10 {
				t.Errorf("without the map the writer's page cache holds %d KiB: the control no longer fills its %d KiB", cache>>10, cacheKiB)
			}
		})
	}
}

// cacheUsed is SQLITE_DBSTATUS_CACHE_USED of the connection c holds: the
// bytes of its page cache.
func cacheUsed(t *testing.T, c *sql.Conn) int {
	t.Helper()
	var n int
	err := c.Raw(func(dc any) error {
		for {
			if s, ok := dc.(msqlite.DBStatus); ok {
				var err error
				n, _, err = s.Status(msqlite.DBStatusCacheUsed, false)
				return err
			}
			u, ok := dc.(interface{ Unwrap() driver.Conn })
			if !ok {
				return fmt.Errorf("%T has no DBStatus", dc)
			}
			dc = u.Unwrap()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
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

// A live hub's render workers, GOMAXPROCS of them by default, hold one
// reader each for good. The default pool leaves at least four readers for
// everything else.
func TestDefaultReadersLeaveRoomBesideTheHub(t *testing.T) {
	ctx := context.Background()
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(0))
	for _, procs := range []int{2, max(runtime.GOMAXPROCS(0), 6)} {
		runtime.GOMAXPROCS(procs)
		d := open(t, Options{})
		if got, want := d.Options().Readers, max(8, procs+4); got != want {
			t.Errorf("GOMAXPROCS %d: %d readers, want %d", procs, got, want)
		}
		conns := make([]*sql.Conn, 0, procs+4)
		for i := range procs + 4 {
			wait, cancel := context.WithTimeout(ctx, time.Second)
			c, err := d.R.Conn(wait)
			cancel()
			if err != nil {
				t.Fatalf("GOMAXPROCS %d: reader %d: %v", procs, i+1, err)
			}
			conns = append(conns, c)
		}
		for _, c := range conns {
			c.Close()
		}
		d.Close()
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

func TestViewValue(t *testing.T) {
	ctx := context.Background()
	d := open(t, Options{})
	if _, err := d.W.Exec("CREATE TABLE t (x INTEGER); INSERT INTO t VALUES (1), (2)"); err != nil {
		t.Fatal(err)
	}
	n, err := d.ViewValue(ctx, func(tx *sql.Tx) (n int, err error) {
		err = tx.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&n)
		return n, err
	})
	if err != nil || n != 2 {
		t.Fatalf("ViewValue = %d, %v; want 2", n, err)
	}
	failed := errors.New("failed")
	n, err = d.ViewValue(ctx, func(*sql.Tx) (int, error) { return 7, failed })
	if n != 0 || err != failed {
		t.Fatalf("a failed ViewValue = %d, %v; want the zero value and its error", n, err)
	}
}

var (
	_ Querier = (*sql.DB)(nil)
	_ Querier = (*sql.Tx)(nil)
	_ Querier = (*sql.Conn)(nil)
)

// A View called with a context that carries a transaction on the same
// database runs in it: a store called from a render sees the render's
// snapshot, even after a commit, and leaves the transaction open.
func TestViewJoinsTheTransactionInCtx(t *testing.T) {
	ctx := context.Background()
	d := open(t, Options{})
	if _, err := d.W.Exec("CREATE TABLE t (x INTEGER); INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if _, ok := TxFrom(ctx); ok {
		t.Fatal("a plain context carries a transaction")
	}
	count := func(ctx context.Context) (n int) {
		t.Helper()
		err := d.View(ctx, func(tx *sql.Tx) error { return tx.QueryRow("SELECT count(*) FROM t").Scan(&n) })
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	err := d.View(ctx, func(tx *sql.Tx) error {
		ctx := WithTx(ctx, tx)
		if got, ok := TxFrom(ctx); !ok || got != tx {
			t.Fatal("TxFrom lost the transaction")
		}
		before := count(ctx)
		if _, err := d.W.Exec("INSERT INTO t VALUES (2)"); err != nil {
			return err
		}
		if after := count(ctx); after != before {
			t.Errorf("a View with the caller's transaction saw %d rows, the caller %d", after, before)
		}
		if err := d.View(ctx, func(*sql.Tx) error { return errors.New("refused") }); err == nil {
			t.Error("the inner View lost its error")
		}
		var n int
		return tx.QueryRow("SELECT count(*) FROM t").Scan(&n)
	})
	if err != nil {
		t.Fatalf("the caller's transaction after the inner Views: %v", err)
	}
	if n := count(ctx); n != 2 {
		t.Fatalf("%d rows after the View", n)
	}
}

func TestQuerierFrom(t *testing.T) {
	ctx := context.Background()
	if _, ok := QuerierFrom(ctx); ok {
		t.Fatal("a plain context carries a Querier")
	}
	d := open(t, Options{})
	err := d.View(ctx, func(tx *sql.Tx) error {
		q, ok := QuerierFrom(WithTx(ctx, tx))
		if !ok || q != Querier(tx) {
			t.Fatalf("QuerierFrom gave %v, %v for the transaction from WithTx", q, ok)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A transaction on another database is not joined: each View reads its
// own database.
func TestViewKeepsToItsDatabase(t *testing.T) {
	ctx := context.Background()
	a, b := open(t, Options{}), open(t, Options{})
	if _, err := b.W.Exec("CREATE TABLE only_b (x INTEGER); INSERT INTO only_b VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	err := a.View(ctx, func(tx *sql.Tx) error {
		return b.View(WithTx(ctx, tx), func(tx *sql.Tx) error {
			var n int
			return tx.QueryRow("SELECT count(*) FROM only_b").Scan(&n)
		})
	})
	if err != nil {
		t.Fatalf("b's View ran in a's transaction: %v", err)
	}

	// The same file, opened twice, is the same database.
	c, err := Open(ctx, a.Path, Options{Synchronous: "NORMAL"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := a.W.Exec("CREATE TABLE t (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	err = a.View(ctx, func(outer *sql.Tx) error {
		return c.View(WithTx(ctx, outer), func(tx *sql.Tx) error {
			if tx != outer {
				t.Error("a second DB on the same file did not join the transaction")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
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

// ReadOnly reads an existing database, here while another program writes
// to it and its last commits are still in the WAL, and a backup of it.
func TestReadOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rw.db")
	rw, err := Open(ctx, path, Options{Synchronous: "NORMAL", Checkpoint: CheckpointExternal})
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	if _, err := rw.W.Exec("CREATE TABLE t (x INTEGER); INSERT INTO t VALUES (1), (2)"); err != nil {
		t.Fatal(err)
	}

	d, err := Open(ctx, path, Options{ReadOnly: true, Readers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if d.W != nil {
		t.Fatal("a read-only database has a write connection")
	}
	count := func(d *DB) (n int) {
		t.Helper()
		err := d.View(ctx, func(tx *sql.Tx) error { return tx.QueryRow("SELECT count(*) FROM t").Scan(&n) })
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(d); n != 2 {
		t.Fatalf("%d rows", n)
	}
	if _, err := d.R.Exec("INSERT INTO t VALUES (3)"); err == nil {
		t.Fatal("wrote through a read-only database")
	}
	dst := filepath.Join(t.TempDir(), "copy.db")
	if err := d.Backup(ctx, dst); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := Open(ctx, dst, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if n := count(c); n != 2 {
		t.Fatalf("the backup has %d rows", n)
	}
}

func TestReadOnlyCreatesNothing(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "missing")
	path := filepath.Join(dir, "x.db")
	if _, err := Open(ctx, path, Options{ReadOnly: true}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing database: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open created the directory: %v", err)
	}
	d := open(t, Options{})
	d.Close()
	_, err := Open(ctx, d.Path, Options{ReadOnly: true, Migrations: files(nil)})
	if err == nil || !strings.Contains(err.Error(), "ReadOnly") {
		t.Fatalf("ReadOnly with migrations: %v", err)
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

// Under CheckpointExternal the last close checkpoints the WAL but keeps
// the file and its header, as Litestream needs to see an unchanged database
// as unchanged when it attaches again: through a wrapping driver too, and
// from a write connection that database/sql opened in place of a bad one.
// The other modes delete the WAL.
func TestCheckpointExternalKeepsTheWAL(t *testing.T) {
	ctx := context.Background()
	wrapped := fmt.Sprintf("sqlite-keepwal-%d", registrations.Add(1))
	if err := dbmetrics.Register(wrapped, "sqlite", dbmetrics.Options{SlowThreshold: -1}); err != nil {
		t.Fatal(err)
	}
	header := func(t *testing.T, path string) []byte {
		t.Helper()
		b, err := os.ReadFile(path + "-wal")
		if err != nil {
			t.Fatalf("the WAL after the close: %v", err)
		}
		if len(b) < 32 {
			t.Fatalf("a WAL of %d bytes", len(b))
		}
		return b[:32]
	}
	for _, tc := range []struct {
		name    string
		opts    Options
		keep    bool
		replace bool
	}{
		{"external", Options{Checkpoint: CheckpointExternal}, true, false},
		{"external through a wrapping driver", Options{Checkpoint: CheckpointExternal, Driver: wrapped}, true, false},
		{"external after a bad connection", Options{Checkpoint: CheckpointExternal}, true, true},
		{"writer", Options{Checkpoint: CheckpointWriter}, false, false},
		{"auto", Options{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "keep.db")
			d, err := Open(ctx, path, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.W.Exec("CREATE TABLE t (x INTEGER); INSERT INTO t VALUES (1), (2)"); err != nil {
				t.Fatal(err)
			}
			if tc.replace {
				// modernc reports an interrupted connection bad, and database/sql
				// closes it and opens another for the next statement.
				c, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
				var n int
				err := d.W.QueryRowContext(c, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT count(*) FROM c").Scan(&n)
				cancel()
				if err == nil {
					t.Fatal("an endless query ended")
				}
				if open := d.W.Stats().OpenConnections; open != 0 {
					t.Fatalf("%d write connections open after the interrupt, want the bad one closed", open)
				}
				if _, err := d.W.Exec("INSERT INTO t VALUES (3)"); err != nil {
					t.Fatal(err)
				}
			}
			var rows int
			if err := d.W.QueryRow("SELECT count(*) FROM t").Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			if !tc.keep {
				if _, err := os.Stat(path + "-wal"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("the WAL after the close: %v, want it removed", err)
				}
				return
			}
			first := header(t, path)

			// Opened again and closed without a write, the WAL keeps its header.
			d, err = Open(ctx, path, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			var n int
			if err := d.View(ctx, func(tx *sql.Tx) error { return tx.QueryRow("SELECT count(*) FROM t").Scan(&n) }); err != nil {
				t.Fatal(err)
			}
			if n != rows {
				t.Fatalf("%d rows after the reopen, want %d", n, rows)
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			if again := header(t, path); string(again) != string(first) {
				t.Fatalf("the WAL's header changed over a reopen without writes:\n%x\n%x", first, again)
			}
		})
	}
}

func TestMemoryDatabaseRefused(t *testing.T) {
	if _, err := Open(context.Background(), ":memory:", Options{Synchronous: "NORMAL"}); err == nil {
		t.Fatal("opened an in-memory database: the pools would not share it")
	}
}

// registrations keeps driver names unique when a test runs more than once
// in one process: sql.Register cannot be undone.
var registrations atomic.Int32

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
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

// Every connection opens through Options.Driver: the write connection, the
// read pool and Backup's. The wrapper here logs every statement as slow.
func TestDriver(t *testing.T) {
	ctx := context.Background()
	var log syncBuffer
	name := fmt.Sprintf("sqlite-timed-%d", registrations.Add(1))
	err := dbmetrics.Register(name, "sqlite", dbmetrics.Options{
		SlowThreshold: time.Nanosecond,
		Logger:        slog.New(slog.NewTextHandler(&log, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	d := open(t, Options{Driver: name})
	if got := d.Options().Driver; got != name {
		t.Errorf("Options().Driver = %q", got)
	}
	if _, err := d.W.Exec("CREATE TABLE t (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.R.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := d.Backup(ctx, filepath.Join(t.TempDir(), "copy.db")); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"CREATE TABLE t", "SELECT count(*) FROM t", "VACUUM INTO"} {
		if !strings.Contains(log.String(), q) {
			t.Errorf("%q did not go through the driver:\n%s", q, log.String())
		}
	}

	if got := open(t, Options{}).Options().Driver; got != "sqlite" {
		t.Errorf("the default driver is %q", got)
	}
	_, err = Open(ctx, filepath.Join(t.TempDir(), "x.db"), Options{Synchronous: "NORMAL", Driver: "no-such-driver"})
	if err == nil || !strings.Contains(err.Error(), "no-such-driver") {
		t.Errorf("an unknown driver: %v", err)
	}
}

// The read pool opens the file read-only: a statement that turns
// query_only off first still cannot write, on any of its connections.
// ATTACH still works, as the DB's doc says: mode=ro covers the main file.
func TestReadPoolIsReadOnly(t *testing.T) {
	ctx := context.Background()
	d := open(t, Options{Readers: 2, Migrations: files(map[string]string{
		"001_t.sql": "CREATE TABLE t (x INTEGER);",
	})})
	if _, err := d.W.Exec(`INSERT INTO t VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO t VALUES (2)`,
		`PRAGMA query_only = 0; INSERT INTO t VALUES (2)`,
		`PRAGMA query_only = OFF; DELETE FROM t`,
	} {
		// Both connections of the pool, held at once.
		cs := make([]*sql.Conn, 2)
		for i := range cs {
			c, err := d.R.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			cs[i] = c
			if _, err := c.ExecContext(ctx, q); err == nil {
				t.Errorf("reader %d wrote: %s", i, q)
			}
		}
		for _, c := range cs {
			c.Close()
		}
	}
	var n int
	if err := d.W.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows: %d, %v", n, err)
	}

	other := open(t, Options{Migrations: files(map[string]string{"001_s.sql": "CREATE TABLE s (y TEXT); INSERT INTO s VALUES ('other');"})}).Path
	c, err := d.R.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, `ATTACH DATABASE ? AS o`, other); err != nil {
		t.Fatalf("ATTACH on a reader: %v", err)
	}
	if _, err := c.ExecContext(ctx, `DETACH DATABASE o`); err != nil {
		t.Fatal(err)
	}
}
