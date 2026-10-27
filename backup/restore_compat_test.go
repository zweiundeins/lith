package backup

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benbjohnson/litestream"
	"github.com/benbjohnson/litestream/file"
	"github.com/pierrec/lz4/v4"
	"github.com/superfly/ltx"
)

// syncEach runs each statement in its own transaction and waits for its
// WAL segment to reach the replica, so that every one is a file of its own.
func syncEach(t *testing.T, m *Manager, name string, db *sql.DB, query string, args ...any) {
	t.Helper()
	mustExec(t, db, query, args...)
	m.mu.Lock()
	ldb := m.attached[name].db
	m.mu.Unlock()
	if err := ldb.SyncAndWait(bg); err != nil {
		t.Fatal(err)
	}
}

// TestRestoreMatchesLitestream restores a replica of 1,300 transactions
// (rows up to 6 KB, updates, deletes, a VACUUM every 250, a snapshot at
// the 150th) at 15 moments and at the latest, with the manager and with
// litestream's Replica.Restore, and compares the files byte for byte. Past
// the snapshot the plan has over 1,000 files: two levels of merges.
func TestRestoreMatchesLitestream(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	mustExec(t, db, `CREATE TABLE blobs (id INTEGER PRIMARY KEY, b BLOB)`)
	const txs = 1300
	times := make([]time.Time, txs)
	for i := range txs {
		switch {
		case i%250 == 249:
			mustExec(t, db, `DELETE FROM blobs WHERE id % 2 = 0`)
			syncEach(t, m, "app", db, `VACUUM`)
		case i%10 == 3:
			syncEach(t, m, "app", db, `UPDATE blobs SET b = randomblob(abs(random()) % 6000) WHERE id % 7 = ?`, i%7)
		case i%10 == 7:
			syncEach(t, m, "app", db, `DELETE FROM blobs WHERE id IN (SELECT id FROM blobs ORDER BY random() LIMIT 2)`)
		default:
			syncEach(t, m, "app", db, `INSERT INTO blobs (b) VALUES (randomblob(abs(random()) % 6000))`)
		}
		if i == 150 {
			m.mu.Lock()
			ldb := m.attached["app"].db
			m.mu.Unlock()
			if _, err := ldb.Snapshot(bg); err != nil {
				t.Fatal(err)
			}
		}
		times[i] = time.Now()
	}
	shutdown(t, db, detach)
	if n := countLevel(t, m, "app", 0); n < 1100 {
		t.Fatalf("level 0 has %d files, want over 1,100", n)
	}

	dir := t.TempDir()
	targets := []time.Time{{}}
	for i := 0; i < 15; i++ {
		targets = append(targets, times[40+i*(txs-41)/14])
	}
	for i, at := range targets {
		ours, theirs := filepath.Join(dir, fmt.Sprintf("ours-%d.db", i)), filepath.Join(dir, fmt.Sprintf("theirs-%d.db", i))
		if err := m.RestoreAt(bg, "app", ours, at); err != nil {
			t.Fatalf("RestoreAt(%v): %v", at, err)
		}
		opt := litestream.NewRestoreOptions()
		opt.OutputPath, opt.Timestamp = theirs, at
		if err := litestream.NewReplicaWithClient(nil, m.newClient("app")).Restore(bg, opt); err != nil {
			t.Fatalf("litestream's Restore(%v): %v", at, err)
		}
		a, err := os.ReadFile(ours)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(theirs)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("target %d (%v): %d bytes restored, litestream's %d, and they differ", i, at, len(a), len(b))
		}
	}
}

// TestRestoreAtAGap: with a WAL segment missing, a restore to a moment
// after it fails instead of stopping at the gap with an older database.
func TestRestoreAtAGap(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	var before time.Time
	for i := range 40 {
		syncEach(t, m, "app", db, `INSERT INTO notes (body) VALUES (?)`, fmt.Sprintf("row %d", i))
		if i == 10 {
			time.Sleep(5 * time.Millisecond)
			before = time.Now()
			time.Sleep(5 * time.Millisecond)
		}
	}
	shutdown(t, db, detach)

	client := m.newClient("app").(*file.ReplicaClient)
	itr, err := client.LTXFiles(bg, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	infos, err := listAll(itr)
	if err != nil {
		t.Fatal(err)
	}
	gone := infos[len(infos)/2]
	if err := os.Remove(client.LTXFilePath(0, gone.MinTXID, gone.MaxTXID)); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := m.Restore(bg, "app", filepath.Join(dir, "latest.db")); err == nil {
		t.Error("Restore of the latest state passed over a gap")
	}
	if err := m.RestoreAt(bg, "app", filepath.Join(dir, "now.db"), time.Now()); err == nil || !strings.Contains(err.Error(), "non-contiguous") {
		t.Errorf("RestoreAt after the gap = %v, want a non-contiguous error", err)
	}
	out := filepath.Join(dir, "before.db")
	if err := m.RestoreAt(bg, "app", out, before); err != nil {
		t.Fatalf("RestoreAt before the gap: %v", err)
	}
	if got := bodies(t, out); len(got) != 12 {
		t.Errorf("restored %d rows before the gap, want 12", len(got))
	}
}

func listAll(itr ltx.FileIterator) ([]*ltx.FileInfo, error) {
	var out []*ltx.FileInfo
	for itr.Next() {
		out = append(out, itr.Item())
	}
	return out, errors.Join(itr.Err(), itr.Close())
}

// writeV3Snapshot writes the database at src as a v0.3.x snapshot of the
// replica under name, created at at.
func writeV3Snapshot(t *testing.T, m *Manager, name, src string, at time.Time) {
	t.Helper()
	dir := filepath.Join(m.cfg.Dir, name, litestream.GenerationsDirV3, "0123456789abcdef", litestream.SnapshotsDirV3)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, litestream.FormatSnapshotFilenameV3(0))
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := lz4.NewWriter(f)
	if _, err := io.Copy(w, in); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// v3Source is a database of three notes, as a v0.3.x snapshot holds one.
func v3Source(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v3.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, `CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`)
	mustExec(t, db, `INSERT INTO notes (body) VALUES ('v3 a'), ('v3 b'), ('v3 c')`)
	return path
}

// TestRestoreV3Replica: a replica with v0.3.x generations restores as
// litestream's own restore does: v0.3.x alone, and in a mixed replica
// whichever format has the newer backup.
func TestRestoreV3Replica(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	src := v3Source(t)
	dir := t.TempDir()

	writeV3Snapshot(t, m, "old", src, time.Now().Add(-time.Hour))
	if err := m.Restore(bg, "old", filepath.Join(dir, "old.db")); err != nil {
		t.Fatalf("Restore of a v0.3.x replica: %v", err)
	}
	if got := bodies(t, filepath.Join(dir, "old.db")); len(got) != 3 || got[0] != "v3 a" {
		t.Errorf("restored %v, want the v0.3.x notes", got)
	}
	missing := filepath.Join(dir, "missing.db")
	if err := m.RestoreIfMissing(bg, "old", missing); err != nil {
		t.Fatal(err)
	}
	if got := bodies(t, missing); len(got) != 3 {
		t.Errorf("RestoreIfMissing restored %v, want the v0.3.x notes", got)
	}
	if err := m.VerifyRestore(bg, "old"); err != nil {
		t.Errorf("VerifyRestore of a v0.3.x replica: %v", err)
	}

	path := filepath.Join(t.TempDir(), "mixed.db")
	db, detach := replicated(t, m, "mixed", path)
	shutdown(t, db, detach)
	writeV3Snapshot(t, m, "mixed", src, time.Now().Add(time.Hour))
	if err := m.Restore(bg, "mixed", filepath.Join(dir, "newer.db")); err != nil {
		t.Fatal(err)
	}
	if got := bodies(t, filepath.Join(dir, "newer.db")); len(got) != 3 || got[0] != "v3 a" {
		t.Errorf("mixed replica with a newer v0.3.x snapshot restored %v", got)
	}
	writeV3Snapshot(t, m, "mixed", src, time.Now().Add(-24*time.Hour))
	if err := m.Restore(bg, "mixed", filepath.Join(dir, "older.db")); err != nil {
		t.Fatal(err)
	}
	if got := bodies(t, filepath.Join(dir, "older.db")); len(got) != 1 || got[0] != "first" {
		t.Errorf("mixed replica with an older v0.3.x snapshot restored %v, want the LTX state", got)
	}
}

// TestRestoreSweepsStaleWorkingDirs: a restore removes the working
// directory a killed one left beside its output, and keeps a fresh one.
func TestRestoreSweepsStaleWorkingDirs(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	shutdown(t, db, detach)

	dir := t.TempDir()
	stale := filepath.Join(dir, ".other.db-restore-123")
	fresh := filepath.Join(dir, ".another.db-restore-456")
	for _, d := range []string{stale, fresh} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "00000001.ltx"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-staleRestoreAge - time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := m.Restore(bg, "app", filepath.Join(dir, "app.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale working directory is still there: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the fresh working directory went: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("left beside the output: %v", entries)
	}
}
