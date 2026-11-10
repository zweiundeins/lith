package backup

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zweiundeins/lith/sqlite"
	msqlite "modernc.org/sqlite"
)

// replicaObjects lists the files under a Dir replica.
func replicaObjects(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// An unchanged database that lith's sqlite.Open opened with
// CheckpointExternal, attached, detached and closed writes no new object to
// its replica when it is opened and attached again: detach restarted the
// WAL through Litestream, the close kept it, and Attach finds the position
// the final sync left. With the WAL emptied after detach, the order these
// docs gave up to v0.5.4, or removed by a close without PERSIST_WAL, every
// cycle uploads the whole database as a new LTX file.
func TestReattachingAnUnchangedDatabaseUploadsNothing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		checkpoint sqlite.Checkpoint
		truncate   bool // PRAGMA wal_checkpoint(TRUNCATE) between detach and close
		want       int  // new replica objects per cycle without writes
	}{
		{"closed after detach", sqlite.CheckpointExternal, false, 0},
		{"truncated after detach", sqlite.CheckpointExternal, true, 1},
		{"WAL removed at close", sqlite.CheckpointWriter, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			m, err := New(Config{Dir: dir, Logger: slog.New(slog.DiscardHandler)})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "app.db")
			// cycle opens, attaches, runs write while attached, detaches and closes.
			cycle := func(write string) {
				t.Helper()
				db, err := sqlite.Open(bg, path, sqlite.Options{Checkpoint: tc.checkpoint})
				if err != nil {
					t.Fatal(err)
				}
				detach, err := m.Attach(bg, "app", path)
				if err != nil {
					t.Fatalf("Attach: %v", err)
				}
				waitForAttachSnapshot(m, "app")
				if write != "" {
					if _, err := db.W.Exec(write); err != nil {
						t.Fatal(err)
					}
				}
				if err := detach(bg); err != nil {
					t.Fatalf("detach: %v", err)
				}
				if tc.truncate {
					if _, err := db.W.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
						t.Fatal(err)
					}
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			db, err := sqlite.Open(bg, path, sqlite.Options{Checkpoint: tc.checkpoint})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.W.Exec(`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			cycle(`INSERT INTO notes (body) VALUES ('first')`)

			before := replicaObjects(t, dir)
			for i := range 3 {
				cycle("")
				after := replicaObjects(t, dir)
				added := slices.DeleteFunc(slices.Clone(after), func(p string) bool { return slices.Contains(before, p) })
				if len(added) != tc.want {
					t.Fatalf("cycle %d without writes added %d objects to the replica, want %d: %v", i+1, len(added), tc.want, added)
				}
				before = after
			}

			// A write after the unchanged cycles still reaches the replica.
			cycle(`INSERT INTO notes (body) VALUES ('second')`)
			out := filepath.Join(t.TempDir(), "restored.db")
			if err := m.Restore(bg, "app", out); err != nil {
				t.Fatal(err)
			}
			if got := bodies(t, out); !slices.Equal(got, []string{"first", "second"}) {
				t.Fatalf("restored %v", got)
			}
		})
	}
}

// A database removed while its -wal and -shm stayed, as a database that
// keeps its WAL leaves them, is restored as the replica holds it: the
// restore removes them, where SQLite would replay the WAL's later writes.
func TestRestoreIfMissingRemovesALeftoverWAL(t *testing.T) {
	m := newManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	if err := detach(bg); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO notes (body) VALUES ('after detach')`)
	closeApp(t, db)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("no WAL left beside the removed database: %v", err)
	}
	if err := m.RestoreIfMissing(bg, "app", path); err != nil {
		t.Fatal(err)
	}
	if got := bodies(t, path); !slices.Equal(got, []string{"first"}) {
		t.Fatalf("restored %v, want the replica's [first]", got)
	}
}

// A database whose WAL grew while attached opens again, after detach and the
// close, with no frames in its WAL to read: detach had Litestream checkpoint
// and restart the WAL. A reader then reads the table through the memory map,
// and its page cache stays small. Closed with that WAL still in place, as
// v0.5.5's stop left it and as a close does without Litestream attached,
// SQLite's recovery at the next open counts every frame as not yet
// checkpointed, and the reader reads those pages into its page cache.
func TestDetachLeavesNoWALFramesForTheNextOpen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		attach bool
	}{
		{"detached and closed", true},
		{"closed without Litestream", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newManager(t)
			path := filepath.Join(t.TempDir(), "app.db")
			opts := sqlite.Options{Checkpoint: sqlite.CheckpointExternal, Readers: 1, CacheKiB: 2000}
			db, err := sqlite.Open(bg, path, opts)
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, db.W, `CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`)
			mustExec(t, db.W, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 8000)
				INSERT INTO notes (body) SELECT printf('%.200c', 'a') FROM n`)
			mustExec(t, db.W, `PRAGMA wal_checkpoint(TRUNCATE)`)
			detach := func(context.Context) error { return nil }
			if tc.attach {
				if detach, err = m.Attach(bg, "app", path); err != nil {
					t.Fatal(err)
				}
				waitForAttachSnapshot(m, "app")
			}
			// Rewrites every page of the table: about 420 frames.
			mustExec(t, db.W, `UPDATE notes SET body = upper(body)`)
			if err := detach(bg); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			db, err = sqlite.Open(bg, path, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			c, err := db.R.Conn(bg)
			if err != nil {
				t.Fatal(err)
			}
			var n int
			if err := c.QueryRowContext(bg, `SELECT count(*) FROM notes WHERE body LIKE 'A%'`).Scan(&n); err != nil || n != 8000 {
				t.Fatalf("read %d rows: %v", n, err)
			}
			cache := cacheUsed(t, c)
			c.Close()
			// The frames SQLite's recovery found, none of them checkpointed since.
			var busy, frames, done int
			if err := db.W.QueryRow(`PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &frames, &done); err != nil {
				t.Fatal(err)
			}
			t.Logf("after the reopen: %d WAL frames, the reader's page cache %d KiB", frames, cache>>10)
			if tc.attach {
				if frames > 1 || cache > 100<<10 {
					t.Errorf("the reopened database had %d WAL frames and its reader cached %d KiB, want at most 1 and 100 KiB", frames, cache>>10)
				}
			} else if frames < 400 || cache < 1<<20 {
				t.Errorf("the reopened database had %d WAL frames and its reader cached %d KiB: the control no longer shows the cost", frames, cache>>10)
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
