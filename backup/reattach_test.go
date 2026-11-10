package backup

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zweiundeins/lith/sqlite"
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
// its replica when it is opened and attached again: the close kept the WAL,
// and Attach finds the position the final sync left. With the WAL emptied
// after detach, the order these docs gave before, every cycle uploads the
// whole database as a new LTX file.
func TestReattachingAnUnchangedDatabaseUploadsNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		truncate bool // PRAGMA wal_checkpoint(TRUNCATE) between detach and close
		want     int  // new replica objects per cycle without writes
	}{
		{"closed after detach", false, 0},
		{"truncated after detach", true, 1},
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
				db, err := sqlite.Open(bg, path, sqlite.Options{Checkpoint: sqlite.CheckpointExternal})
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
			db, err := sqlite.Open(bg, path, sqlite.Options{Checkpoint: sqlite.CheckpointExternal})
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
