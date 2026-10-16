package events

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

func TestShadowDDL(t *testing.T) {
	for _, c := range []struct{ sql, want string }{
		{`CREATE TABLE card_titles (id INTEGER PRIMARY KEY, title TEXT) STRICT`,
			`CREATE TABLE "card_titles__v2" (id INTEGER PRIMARY KEY, title TEXT) STRICT`},
		{`CREATE TABLE "card_titles"(id)`, `CREATE TABLE "card_titles__v2"(id)`},
		{`CREATE TABLE IF NOT EXISTS main.card_titles (a)`, `CREATE TABLE IF NOT EXISTS "card_titles__v2" (a)`},
		{`CREATE INDEX card_titles_title ON card_titles (title)`,
			`CREATE INDEX "card_titles_title__v2" ON "card_titles__v2" (title)`},
		{`CREATE UNIQUE INDEX "by title__v1" ON [card_titles](title) WHERE title != 'ON x'`,
			`CREATE UNIQUE INDEX "by title__v2" ON "card_titles__v2"(title) WHERE title != 'ON x'`},
		{"create /* on */ index -- on x\n\"a\"\"b\" on `card_titles` (title)",
			"create /* on */ index -- on x\n\"a\"\"b__v2\" on \"card_titles__v2\" (title)"},
		{`CREATE VIRTUAL TABLE card_titles USING fts5(title)`, `CREATE VIRTUAL TABLE "card_titles__v2" USING fts5(title)`},
	} {
		got, err := shadowDDL(c.sql, shadowName("card_titles", 2), 2)
		if err != nil || got != c.want {
			t.Errorf("%s\ngave %s, %v\nwant %s", c.sql, got, err, c.want)
		}
	}
	for _, sql := range []string{
		`CREATE TRIGGER t AFTER INSERT ON card_titles BEGIN SELECT 1; END`,
		`CREATE VIEW v AS SELECT 1`,
		`SELECT 1`,
		`CREATE TABLE`,
		`CREATE INDEX i card_titles (a)`,
	} {
		if got, err := shadowDDL(sql, "x__v2", 2); err == nil {
			t.Errorf("%s gave %s", sql, got)
		}
	}
}

// The copy's statements create a table and an index of the names a
// rebuild expects, whatever the names hold.
func FuzzShadowDDL(f *testing.F) {
	f.Add("card_titles", "card_titles_title", true)
	f.Add(`a "quoted" table`, "idx__v7", false)
	f.Add("ON", "INDEX", false)
	f.Add("t", "t]", false)
	f.Fuzz(func(t *testing.T, table, index string, bare bool) {
		if table == "" || index == "" || !utf8.ValidString(table+index) || strings.ContainsRune(table+index, 0) ||
			strings.HasPrefix(strings.ToLower(table+index), "sqlite_") {
			t.Skip()
		}
		copy := shadowName(table, 3)
		names := map[string]bool{}
		for _, n := range []string{table, index, copy, shadowName(index, 3)} {
			names[strings.ToLower(n)] = true
		}
		if len(names) < 4 {
			t.Skip()
		}
		name := quote
		if bare && isPlain(table) && isPlain(index) {
			name = func(s string) string { return s }
		}
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		ctx := context.Background()
		ddl := []string{
			`CREATE TABLE ` + name(table) + ` (a INTEGER PRIMARY KEY, b TEXT)`,
			`CREATE INDEX ` + name(index) + ` ON ` + name(table) + ` (b)`,
		}
		for _, q := range ddl {
			if _, err := db.ExecContext(ctx, q); err != nil {
				t.Skip(err) // not a name SQLite takes
			}
		}
		for _, q := range ddl {
			s, err := shadowDDL(q, copy, 3)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			if _, err := db.ExecContext(ctx, s); err != nil {
				t.Fatalf("%s gave %s: %v", q, s, err)
			}
		}
		var got string
		err = db.QueryRowContext(ctx, `SELECT tbl_name FROM sqlite_schema WHERE type = 'index' AND name = ?`, shadowName(index, 3)).Scan(&got)
		if err != nil || got != copy {
			t.Fatalf("the copy's index is on %q, %v", got, err)
		}
	})
}

// isPlain reports whether s needs no quotes: a word that is no keyword
// the tests' statements use.
func isPlain(s string) bool {
	if s == "" || s[0] >= '0' && s[0] <= '9' {
		return false
	}
	for i := range len(s) {
		if c := s[i]; !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	switch strings.ToUpper(s) {
	case "ON", "INDEX", "TABLE", "IF", "NOT", "EXISTS", "CREATE", "UNIQUE", "TEMP", "TEMPORARY", "VIRTUAL":
		return false
	}
	return true
}
