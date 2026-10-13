package migrate_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"

	"github.com/zweiundeins/lith/site/internal/migrate"
)

func open(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func files(m map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range m {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func tables(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	return strings.Join(names, " ")
}

// TestScopes applies two scopes that both number from 001, then a new file
// in one of them, and nothing twice.
func TestScopes(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	site := files(map[string]string{"001_site.sql": `CREATE TABLE site_a (id INTEGER PRIMARY KEY);`})
	demo := files(map[string]string{"001_init.sql": `CREATE TABLE demo_a (id INTEGER PRIMARY KEY);`})
	scopes := []migrate.Scope{{Name: "site", FS: site}, {Name: "demo", FS: demo}}
	if err := migrate.Run(ctx, db, scopes); err != nil {
		t.Fatal(err)
	}
	if got := tables(t, db); got != "demo_a site_a site_migrations" {
		t.Fatalf("tables: %s", got)
	}
	demo["002_more.sql"] = &fstest.MapFile{Data: []byte(`CREATE TABLE demo_b (id INTEGER PRIMARY KEY);`)}
	if err := migrate.Run(ctx, db, scopes); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, db, scopes); err != nil { // nothing left to apply
		t.Fatal(err)
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM site_migrations WHERE scope = 'demo'`).Scan(&n)
	if n != 2 || !strings.Contains(tables(t, db), "demo_b") {
		t.Fatalf("demo has %d migrations recorded, tables %s", n, tables(t, db))
	}
}

// TestNewerDatabase refuses to run a program whose files are older than
// the database.
func TestNewerDatabase(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	two := files(map[string]string{"001_a.sql": `CREATE TABLE x_a (id INTEGER);`, "002_b.sql": `CREATE TABLE x_b (id INTEGER);`})
	if err := migrate.Run(ctx, db, []migrate.Scope{{Name: "x", FS: two}}); err != nil {
		t.Fatal(err)
	}
	one := files(map[string]string{"001_a.sql": `CREATE TABLE x_a (id INTEGER);`})
	if err := migrate.Run(ctx, db, []migrate.Scope{{Name: "x", FS: one}}); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("an older program ran: %v", err)
	}
}

// TestFailedMigration leaves nothing of a migration that fails, and
// records nothing for it.
func TestFailedMigration(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	bad := files(map[string]string{"001_a.sql": `CREATE TABLE x_a (id INTEGER); CREATE TABLE x_a (id INTEGER);`})
	if err := migrate.Run(ctx, db, []migrate.Scope{{Name: "x", FS: bad}}); err == nil {
		t.Fatal("a failing migration passed")
	}
	if got := tables(t, db); got != "site_migrations" {
		t.Fatalf("tables after a failed migration: %s", got)
	}
}

func TestFileNames(t *testing.T) {
	for name, files := range map[string]fstest.MapFS{
		"no number":   {"init.sql": {}},
		"zero":        {"000_init.sql": {}},
		"same number": {"001_a.sql": {}, "001_b.sql": {}},
		"capitals":    {"001_Init.sql": {}},
	} {
		if _, err := migrate.Files(files); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := migrate.Run(context.Background(), open(t), []migrate.Scope{{Name: "a"}, {Name: "a"}}); err == nil {
		t.Error("a scope listed twice was accepted")
	}
}
