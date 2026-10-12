package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func files(m map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range m {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func openAt(path string, migrations map[string]string) (*DB, error) {
	return Open(context.Background(), path, Options{Synchronous: "NORMAL", Migrations: files(migrations)})
}

func scalar(t *testing.T, d *DB, q string) int {
	t.Helper()
	var n int
	if err := d.R.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestMigrationsRunInNumericOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	ms := map[string]string{
		"1_create.sql":   "CREATE TABLE t (a INTEGER);",
		"2_column.sql":   "ALTER TABLE t ADD COLUMN b INTEGER;",
		"10_insert.sql":  "INSERT INTO t (a, b) VALUES (1, 2);",
		"README.md":      "not a migration",
		"notes/1_x.sql":  "not at the root",
		"003_index.sql":  "CREATE INDEX t_b ON t (b);",
		"009_second.sql": "INSERT INTO t (a, b) VALUES (0, 0); DELETE FROM t WHERE a = 0;",
	}
	d, err := openAt(path, ms)
	if err != nil {
		t.Fatal(err)
	}
	if v := scalar(t, d, "PRAGMA user_version"); v != 10 {
		t.Fatalf("user_version %d, want 10", v)
	}
	if n := scalar(t, d, "SELECT count(*) FROM t WHERE b = 2"); n != 1 {
		t.Fatalf("%d rows", n)
	}
	d.Close()

	// Opening again applies only what is new.
	ms["11_more.sql"] = "INSERT INTO t (a, b) VALUES (3, 4);"
	d, err = openAt(path, ms)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if v := scalar(t, d, "PRAGMA user_version"); v != 11 {
		t.Fatalf("user_version %d, want 11", v)
	}
	if n := scalar(t, d, "SELECT count(*) FROM t"); n != 2 {
		t.Fatalf("%d rows: a migration ran twice", n)
	}
}

func TestFailedMigrationLeavesTheLastGoodVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	_, err := openAt(path, map[string]string{
		"1_create.sql": "CREATE TABLE t (a INTEGER);",
		"2_broken.sql": "INSERT INTO t VALUES (1); INSERT INTO missing VALUES (1);",
	})
	if err == nil || !strings.Contains(err.Error(), "2_broken.sql") {
		t.Fatalf("got %v", err)
	}
	d, err := openAt(path, map[string]string{"1_create.sql": "CREATE TABLE t (a INTEGER);"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if v := scalar(t, d, "PRAGMA user_version"); v != 1 {
		t.Fatalf("user_version %d, want 1", v)
	}
	if n := scalar(t, d, "SELECT count(*) FROM t"); n != 0 {
		t.Fatal("the failed migration's first statement was kept")
	}
}

func TestNewerDatabaseRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	d, err := openAt(path, map[string]string{"1_a.sql": "CREATE TABLE a (x);", "2_b.sql": "CREATE TABLE b (x);"})
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	_, err = openAt(path, map[string]string{"1_a.sql": "CREATE TABLE a (x);"})
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("an older program opened the database: %v", err)
	}
	if _, err := openAt(path, map[string]string{}); err == nil {
		t.Fatal("opened with no migrations at all")
	}
}

func TestMigrationNames(t *testing.T) {
	for _, ms := range []map[string]string{
		{"create.sql": ""},
		{"x1_create.sql": ""},
		{"+1_create.sql": ""},
		{"0_create.sql": ""},
		{"1.sql": ""},
		{"1_a.sql": "", "001_b.sql": ""},
	} {
		if _, err := openAt(filepath.Join(t.TempDir(), "m.db"), ms); err == nil {
			t.Errorf("accepted %v", ms)
		}
	}
}

// Rebuilding a parent table, as SQLite's guide to schema changes does it,
// keeps the rows that reference it: foreign keys are off meanwhile.
func TestMigrationRebuildsParentTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	ms := map[string]string{
		"1_init.sql": `CREATE TABLE parent (id INTEGER PRIMARY KEY, name TEXT);
			CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES parent (id) ON DELETE CASCADE);
			INSERT INTO parent VALUES (1, 'a'), (2, 'b');
			INSERT INTO child VALUES (10, 1), (11, 2);`,
		"2_rebuild.sql": `CREATE TABLE parent_new (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
			INSERT INTO parent_new SELECT id, name FROM parent;
			DROP TABLE parent;
			ALTER TABLE parent_new RENAME TO parent;`,
	}
	d, err := openAt(path, ms)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if n := scalar(t, d, "SELECT count(*) FROM child"); n != 2 {
		t.Fatalf("%d children left after the rebuild", n)
	}
	if got := pragma(t, d.W, "foreign_keys"); got != "1" {
		t.Fatalf("foreign keys stayed off on the write connection: %s", got)
	}
	// The constraint holds again.
	if _, err := d.W.Exec("INSERT INTO child VALUES (12, 99)"); err == nil {
		t.Fatal("inserted a child of a missing parent")
	}
}

func TestMigrationWithForeignKeyViolationFails(t *testing.T) {
	_, err := openAt(filepath.Join(t.TempDir(), "m.db"), map[string]string{
		"1_init.sql": `CREATE TABLE parent (id INTEGER PRIMARY KEY);
			CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent (id));
			INSERT INTO child VALUES (1, 42);`,
	})
	if err == nil || !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("got %v", err)
	}
}
