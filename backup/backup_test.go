package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benbjohnson/litestream"
	_ "modernc.org/sqlite"
)

var bg = context.Background()

// newManager replicates to a temporary directory: the production code path
// with litestream's file client in place of S3's.
func newManager(t *testing.T) *Manager {
	t.Helper()
	m, err := New(Config{Dir: t.TempDir(), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// openApp opens path the way the package doc tells applications to.
func openApp(t *testing.T, m *Manager, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=wal_autocheckpoint(%d)",
		path, m.WALAutocheckpoint()))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	return db
}

// closeApp is the last step of a shutdown, after detach.
func closeApp(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Error(err)
	}
	if err := db.Close(); err != nil {
		t.Error(err)
	}
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// replicated opens a database at path with one row, attached under name.
func replicated(t *testing.T, m *Manager, name, path string) (*sql.DB, func(context.Context) error) {
	t.Helper()
	db := openApp(t, m, path)
	mustExec(t, db, `CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`)
	mustExec(t, db, `INSERT INTO notes (body) VALUES ('first')`)
	detach, err := m.Attach(bg, name, path)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	return db, detach
}

// shutdown detaches and closes in the documented order.
func shutdown(t *testing.T, db *sql.DB, detach func(context.Context) error) {
	t.Helper()
	if err := detach(bg); err != nil {
		t.Errorf("detach: %v", err)
	}
	closeApp(t, db)
}

func removeDB(t *testing.T, path string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
}

func bodies(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT body FROM notes ORDER BY id`)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func countLevel(t *testing.T, m *Manager, name string, level int) int {
	t.Helper()
	itr, err := m.newClient(name).LTXFiles(bg, level, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	defer itr.Close()
	n := 0
	for itr.Next() {
		n++
	}
	return n
}

// writeAndWaitLevel0 inserts rows and waits until a new WAL segment has
// reached the replica, which takes a monitor tick.
func writeAndWaitLevel0(t *testing.T, m *Manager, db *sql.DB, name string, rows int) {
	t.Helper()
	before := countLevel(t, m, name, 0)
	for i := range rows {
		mustExec(t, db, `INSERT INTO notes (body) VALUES (?)`, fmt.Sprintf("row %d.%d", before, i))
	}
	waitFor(t, 10*time.Second, func() bool { return countLevel(t, m, name, 0) > before })
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v", d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDisabledManagerDoesNothing(t *testing.T) {
	m, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Enabled() || m.WALAutocheckpoint() != 1000 {
		t.Fatalf("Enabled = %v, WALAutocheckpoint = %d; want false, 1000", m.Enabled(), m.WALAutocheckpoint())
	}
	path := filepath.Join(t.TempDir(), "app.db")
	if err := m.RestoreIfMissing(bg, "app", path); err != nil {
		t.Errorf("RestoreIfMissing: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Error("RestoreIfMissing created a file")
	}
	detach, err := m.Attach(bg, "app", path)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := detach(bg); err != nil {
		t.Errorf("detach: %v", err)
	}
	if err := m.PurgeReplica(bg, "app"); err != nil {
		t.Errorf("PurgeReplica: %v", err)
	}
	if got := m.MaintainAll(bg, time.Now()); len(got) != 0 {
		t.Errorf("MaintainAll = %v, want nothing", got)
	}
	if got := m.Status(); len(got) != 0 {
		t.Errorf("Status = %v, want nothing", got)
	}
	for op, err := range map[string]error{
		"Restore":       m.Restore(bg, "app", path),
		"VerifyRestore": m.VerifyRestore(bg, "app"),
	} {
		if !errors.Is(err, ErrDisabled) {
			t.Errorf("%s = %v, want ErrDisabled", op, err)
		}
	}
	if _, _, err := m.RestoreWindow(bg, "app"); !errors.Is(err, ErrDisabled) {
		t.Errorf("RestoreWindow = %v, want ErrDisabled", err)
	}
	if _, err := m.RestoreTimeline(bg, "app"); !errors.Is(err, ErrDisabled) {
		t.Errorf("RestoreTimeline = %v, want ErrDisabled", err)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	for _, cfg := range []Config{
		{Bucket: "b", Dir: "/tmp/x"},
		{Bucket: "b", RetentionWindow: -time.Hour},
		{Bucket: "b", Prefix: "../escape"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
	m, err := New(Config{Bucket: "b", RetentionWindow: 48 * time.Hour})
	if err != nil || m.RetentionWindow() != 48*time.Hour || !m.Enabled() || m.WALAutocheckpoint() != 0 {
		t.Errorf("New with a bucket = %v, %v", m, err)
	}
	if m, _ := New(Config{Bucket: "b"}); m.RetentionWindow() != DefaultRetentionWindow {
		t.Errorf("RetentionWindow = %v, want the default", m.RetentionWindow())
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("APP_BACKUP_BUCKET", "bucket")
	t.Setenv("APP_BACKUP_ENDPOINT", "https://s3.example")
	t.Setenv("APP_BACKUP_REGION", "ch-1")
	t.Setenv("APP_BACKUP_ACCESS_KEY_ID", "id")
	t.Setenv("APP_BACKUP_SECRET_ACCESS_KEY", "secret")
	t.Setenv("APP_BACKUP_FORCE_PATH_STYLE", "true")
	t.Setenv("APP_BACKUP_PREFIX", "prod")
	t.Setenv("APP_BACKUP_RETENTION", "720h")
	t.Setenv("APP_BACKUP_SNAPSHOT_INTERVAL", "12h")
	cfg, err := ConfigFromEnv("APP_BACKUP_")
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Bucket: "bucket", Endpoint: "https://s3.example", Region: "ch-1",
		AccessKeyID: "id", SecretAccessKey: "secret", ForcePathStyle: true,
		Prefix: "prod", RetentionWindow: 720 * time.Hour, SnapshotInterval: 12 * time.Hour,
	}
	if cfg != want {
		t.Errorf("ConfigFromEnv = %+v, want %+v", cfg, want)
	}

	t.Setenv("APP_BACKUP_RETENTION", "a month")
	if _, err := ConfigFromEnv("APP_BACKUP_"); err == nil || !strings.Contains(err.Error(), "APP_BACKUP_RETENTION") {
		t.Errorf("ConfigFromEnv with a bad duration = %v, want an error naming the variable", err)
	}
}

func TestReplicaNamesAreOnePathElement(t *testing.T) {
	m := newManager(t)
	for _, name := range []string{"", ".", "..", "../x", "/abs", "a/b", `a\b`} {
		if _, err := m.Attach(bg, name, "unused"); err == nil {
			t.Errorf("Attach accepted the name %q", name)
		}
	}
	for _, prefix := range []string{"../x", "/abs", "a//b", "a/../b", "."} {
		if _, err := New(Config{Dir: t.TempDir(), Prefix: prefix}); err == nil {
			t.Errorf("New accepted the prefix %q", prefix)
		}
	}
}

// TestPrefixesKeepDeploymentsApart: two deployments share a bucket, and a
// purge in one leaves the other's replica of the same name alone.
func TestPrefixesKeepDeploymentsApart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	managers := map[string]*Manager{}
	for _, prefix := range []string{"staging", "prod/ch"} {
		m, err := New(Config{Dir: dir, Prefix: prefix, Logger: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
		shutdown(t, db, detach)
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(prefix), "app")); err != nil {
			t.Fatalf("no replica under %s: %v", prefix, err)
		}
		managers[prefix] = m
	}
	if err := managers["staging"].PurgeReplica(bg, "app"); err != nil {
		t.Fatal(err)
	}
	if err := managers["prod/ch"].VerifyRestore(bg, "app"); err != nil {
		t.Errorf("the other deployment's replica after a purge: %v", err)
	}
}

// TestRoundTripAfterDataLoss is the reason the package exists: rows written
// right before detach, without waiting for a monitor tick, come back after
// the local files are lost.
func TestRoundTripAfterDataLoss(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	if err := m.RestoreIfMissing(bg, "app", path); err != nil {
		t.Fatalf("RestoreIfMissing on an empty replica: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("RestoreIfMissing created a file from an empty replica")
	}

	db, detach := replicated(t, m, "app", path)
	mustExec(t, db, `INSERT INTO notes (body) VALUES ('just before shutdown')`)
	shutdown(t, db, detach)
	removeDB(t, path)

	if err := m.RestoreIfMissing(bg, "app", path); err != nil {
		t.Fatalf("RestoreIfMissing: %v", err)
	}
	if got := bodies(t, path); strings.Join(got, ",") != "first,just before shutdown" {
		t.Errorf("restored rows = %v", got)
	}
}

func TestRestoreIfMissingLeavesAnExistingFileAlone(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	if err := detach(bg); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO notes (body) VALUES ('local only')`)
	closeApp(t, db)

	if err := m.RestoreIfMissing(bg, "app", path); err != nil {
		t.Fatal(err)
	}
	if got := bodies(t, path); len(got) != 2 {
		t.Errorf("rows = %v, want the local file untouched", got)
	}
}

func TestDetachWithACancelledContextStillSyncs(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	mustExec(t, db, `INSERT INTO notes (body) VALUES ('at shutdown')`)

	ctx, cancel := context.WithCancel(bg)
	cancel()
	if err := detach(ctx); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if err := detach(bg); err != nil {
		t.Errorf("a second detach = %v, want the first result", err)
	}
	closeApp(t, db)

	out := filepath.Join(t.TempDir(), "restored.db")
	if err := m.Restore(bg, "app", out); err != nil {
		t.Fatal(err)
	}
	if got := bodies(t, out); len(got) != 2 {
		t.Errorf("restored rows = %v, want the row written before the cancelled detach", got)
	}
}

func TestAttachRefusesASecondReplicator(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	defer shutdown(t, db, detach)

	if _, err := m.Attach(bg, "app", path); err == nil || !strings.Contains(err.Error(), "already attached") {
		t.Errorf("a second Attach = %v, want already attached", err)
	}
	if _, err := m.Attach(bg, "missing", filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Error("Attach of a file that does not exist succeeded")
	}
}

func TestStatusReportsAttachedReplicas(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)

	st := m.Status()
	if len(st) != 1 || st[0].Name != "app" || st[0].Path != path {
		t.Fatalf("Status = %+v, want the one replica", st)
	}
	if st[0].LastSyncAt.IsZero() || st[0].TXID == "" {
		t.Errorf("Status = %+v, want the first sync recorded", st[0])
	}
	shutdown(t, db, detach)
	if st := m.Status(); len(st) != 0 {
		t.Errorf("Status after detach = %+v, want nothing", st)
	}
}

func TestPurgeReplica(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)

	if err := m.PurgeReplica(bg, "app"); err == nil || !strings.Contains(err.Error(), "still attached") {
		t.Fatalf("PurgeReplica while attached = %v, want a refusal", err)
	}
	if err := m.VerifyRestore(bg, "app"); err != nil {
		t.Fatal(err)
	}
	shutdown(t, db, detach)

	if err := m.PurgeReplica(bg, "app"); err != nil {
		t.Fatalf("PurgeReplica: %v", err)
	}
	if n := countLevel(t, m, "app", 0); n != 0 {
		t.Errorf("%d WAL segments left after the purge", n)
	}
	m.mu.Lock()
	_, kept := m.verify["app"]
	m.mu.Unlock()
	if kept {
		t.Error("the purged replica's verify result is still recorded")
	}
	removeDB(t, path)
	if err := m.RestoreIfMissing(bg, "app", path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a purged replica still restored")
	}
	if err := m.PurgeReplica(bg, "never"); err != nil {
		t.Errorf("PurgeReplica of a name never replicated = %v, want nil", err)
	}
}

func TestMetaPathIsTheDirectoryLitestreamCreates(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	defer shutdown(t, db, detach)

	if want := litestream.NewDB(path).MetaPath(); MetaPath(path) != want {
		t.Errorf("MetaPath = %q, litestream says %q", MetaPath(path), want)
	}
	if info, err := os.Stat(MetaPath(path)); err != nil || !info.IsDir() {
		t.Errorf("MetaPath %q is not a directory after Attach: %v", MetaPath(path), err)
	}
}

func TestRestoreAtAndTheWindow(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	if _, _, err := m.RestoreWindow(bg, "app"); !errors.Is(err, ErrNothingToRestore) {
		t.Errorf("RestoreWindow of an empty replica = %v, want ErrNothingToRestore", err)
	}
	if _, err := m.RestoreTimeline(bg, "app"); !errors.Is(err, ErrNothingToRestore) {
		t.Errorf("RestoreTimeline of an empty replica = %v, want ErrNothingToRestore", err)
	}
	if err := m.Restore(bg, "app", filepath.Join(t.TempDir(), "x.db")); !errors.Is(err, ErrNothingToRestore) {
		t.Errorf("Restore of an empty replica = %v, want ErrNothingToRestore", err)
	}

	db, detach := replicated(t, m, "app", path)
	writeAndWaitLevel0(t, m, db, "app", 2)
	shutdown(t, db, detach)

	earliest, latest, err := m.RestoreWindow(bg, "app")
	if err != nil || earliest.IsZero() || latest.Before(earliest) {
		t.Fatalf("RestoreWindow = %v, %v, %v", earliest, latest, err)
	}
	tl, err := m.RestoreTimeline(bg, "app")
	if err != nil || len(tl.Points) < 2 || !tl.Earliest.Equal(earliest) {
		t.Fatalf("RestoreTimeline = %+v, %v", tl, err)
	}
	for i, p := range tl.Points {
		if i > 0 && p.At.Before(tl.Points[i-1].At) {
			t.Errorf("timeline point %d is out of order", i)
		}
	}
	if !tl.Points[0].Snapshot {
		t.Error("the first point, the baseline, is not marked a snapshot")
	}

	out := filepath.Join(t.TempDir(), "latest.db")
	if err := m.RestoreAt(bg, "app", out, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := bodies(t, out); len(got) != 3 {
		t.Errorf("restored rows = %v, want 3", got)
	}
	err = m.RestoreAt(bg, "app", filepath.Join(t.TempDir(), "old.db"), earliest.Add(-24*time.Hour))
	if !errors.Is(err, ErrNothingToRestore) {
		t.Errorf("RestoreAt before the window = %v, want ErrNothingToRestore", err)
	}
}

func TestVerifyRestore(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	if err := m.VerifyRestore(bg, "never"); !errors.Is(err, ErrNothingToRestore) {
		t.Errorf("VerifyRestore of an empty replica = %v, want ErrNothingToRestore", err)
	}
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	defer shutdown(t, db, detach)

	if got := m.VerifyAll(bg); len(got) != 1 || got["app"] != nil {
		t.Fatalf("VerifyAll = %v, want app passing", got)
	}
	if st := m.Status(); !st[0].VerifyOK || st[0].LastVerifyAt.IsZero() {
		t.Errorf("Status = %+v, want the passed verification", st[0])
	}
}

// TestVerifyRestoreCatchesBrokenBytes: attaching without error is no
// evidence the replica can be restored.
func TestVerifyRestoreCatchesBrokenBytes(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	defer shutdown(t, db, detach)

	broken := 0
	err := filepath.WalkDir(m.cfg.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".ltx" {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		broken++
		return os.Truncate(p, info.Size()/2)
	})
	if err != nil || broken == 0 {
		t.Fatalf("truncated %d replica files: %v", broken, err)
	}
	if err := m.VerifyRestore(bg, "app"); err == nil {
		t.Fatal("VerifyRestore passed on a truncated replica")
	}
	if st := m.Status(); st[0].VerifyOK || st[0].LastVerifyAt.IsZero() {
		t.Errorf("Status = %+v, want the failed verification", st[0])
	}
}

// TestLitestreamCheckpointsForTheApp: with wal_autocheckpoint=0 on the
// application's connection the WAL only grows, until Litestream is attached
// and checkpoints it.
func TestLitestreamCheckpointsForTheApp(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	db := openApp(t, m, path)
	var pages int
	if err := db.QueryRow(`PRAGMA wal_autocheckpoint`).Scan(&pages); err != nil || pages != 0 {
		t.Fatalf("wal_autocheckpoint = %d, %v; want 0", pages, err)
	}
	// About 1500 pages, past Litestream's checkpoint threshold of 1000.
	mustExec(t, db, `CREATE TABLE blobs (b BLOB)`)
	mustExec(t, db, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 1500)
		INSERT INTO blobs SELECT randomblob(3000) FROM n`)
	size := func() int64 {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	if s := size(); s > 1<<20 {
		t.Fatalf("the database file is %d bytes, so something checkpointed before Litestream", s)
	}

	detach, err := m.Attach(bg, "app", path)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(t, db, detach)
	waitFor(t, 15*time.Second, func() bool { return size() > 4<<20 })
}
