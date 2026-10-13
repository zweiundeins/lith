// Package migrate applies the site's schema in scopes: the site's own
// migrations, then each demo's, every scope numbered from 001 on its own.
// Demos built on parallel branches never clash on a number, since a number
// only counts within its scope. site_migrations records what ran.
package migrate

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Scope is a set of migrations with a numbering of its own.
type Scope struct {
	// Name is "site" or a demo's slug.
	Name string
	// FS holds NNN_name.sql files at its root; nil holds none.
	FS fs.FS
}

// File is one migration of a scope.
type File struct {
	N    int
	Name string
}

var fileName = regexp.MustCompile(`^([0-9]{3,})_[a-z0-9_]+\.sql$`)

// Files lists a scope's migrations in order. Every .sql file must be named
// like 001_name.sql, and no two may share a number.
func Files(fsys fs.FS) ([]File, error) {
	if fsys == nil {
		return nil, nil
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var files []File
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		m := fileName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration %s: want a name like 001_name.sql", e.Name())
		}
		n, _ := strconv.Atoi(m[1])
		if n == 0 {
			return nil, fmt.Errorf("migration %s: numbers start at 001", e.Name())
		}
		if other, dup := seen[n]; dup {
			return nil, fmt.Errorf("migrations %s and %s have the same number", other, e.Name())
		}
		seen[n] = e.Name()
		files = append(files, File{N: n, Name: e.Name()})
	}
	slices.SortFunc(files, func(a, b File) int { return cmp.Compare(a.N, b.N) })
	return files, nil
}

const ledger = `CREATE TABLE IF NOT EXISTS site_migrations (
    scope   TEXT NOT NULL,
    version INTEGER NOT NULL,
    name    TEXT NOT NULL,
    applied INTEGER NOT NULL, -- unix seconds
    PRIMARY KEY (scope, version)
) WITHOUT ROWID`

// Run applies, scope by scope in the order given, each migration numbered
// above the newest one the scope has applied, each in a transaction of its
// own that also records it. It refuses a database on which a scope is at a
// version newer than its newest file: an older program must not run on a
// newer schema. Foreign keys are off meanwhile, as SQLite's procedure for
// schema changes asks; foreign_key_check runs before each commit instead.
// db is the write connection, as sqlite.Options.Migrate passes it.
func Run(ctx context.Context, db *sql.DB, scopes []Scope) (err error) {
	names := map[string]bool{}
	for _, s := range scopes {
		if s.Name == "" || names[s.Name] {
			return fmt.Errorf("migrate: scope %q is empty or listed twice", s.Name)
		}
		names[s.Name] = true
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, ledger); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	defer func() {
		if _, e := conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA foreign_keys = ON"); e != nil {
			err = errors.Join(err, e)
		}
	}()
	for _, s := range scopes {
		files, err := Files(s.FS)
		if err != nil {
			return fmt.Errorf("migrate: %s: %w", s.Name, err)
		}
		var current int
		if err := conn.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM site_migrations WHERE scope = ?`, s.Name).Scan(&current); err != nil {
			return err
		}
		newest := 0
		if len(files) > 0 {
			newest = files[len(files)-1].N
		}
		if current > newest {
			return fmt.Errorf("migrate: %s is at version %d, newer than its newest migration (%d): refusing to run an older program on it", s.Name, current, newest)
		}
		for _, f := range files {
			if f.N <= current {
				continue
			}
			body, err := fs.ReadFile(s.FS, f.Name)
			if err != nil {
				return err
			}
			if err := apply(ctx, conn, s.Name, f, string(body)); err != nil {
				return fmt.Errorf("migrate: %s/%s: %w", s.Name, f.Name, err)
			}
		}
	}
	return nil
}

func apply(ctx context.Context, conn *sql.Conn, scope string, f File, body string) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, body); err != nil {
		return err
	}
	if err := foreignKeyCheck(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO site_migrations (scope, version, name, applied) VALUES (?, ?, ?, ?)`,
		scope, f.N, f.Name, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func foreignKeyCheck(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fk int
		if err := rows.Scan(&table, &rowid, &parent, &fk); err != nil {
			return err
		}
		return fmt.Errorf("foreign key violation: a row of %s (rowid %d) references a missing row of %s", table, rowid.Int64, parent)
	}
	return rows.Err()
}
