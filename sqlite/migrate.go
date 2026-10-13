package sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
)

type migration struct {
	n    int
	name string
}

// migrations lists the NNN_name.sql files at the root of fsys in order.
func migrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("sqlite: migrations: %w", err)
	}
	var ms []migration
	seen := map[int]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		num, _, ok := strings.Cut(name, "_")
		n, err := strconv.Atoi(num)
		if !ok || err != nil || n <= 0 || strings.TrimLeft(num, "0123456789") != "" {
			return nil, fmt.Errorf("sqlite: migration %s: want a name like 001_name.sql", name)
		}
		if other, dup := seen[n]; dup {
			return nil, fmt.Errorf("sqlite: migrations %s and %s have the same number", other, name)
		}
		seen[n] = name
		ms = append(ms, migration{n: n, name: name})
	}
	slices.SortFunc(ms, func(a, b migration) int { return cmp.Compare(a.n, b.n) })
	return ms, nil
}

// Migrate applies the NNN_name.sql files at the root of fsys that are
// numbered above the database's PRAGMA user_version, as Open does with
// Options.Migrations: in order, each in a transaction of its own, and it
// refuses a database whose version is newer than the newest file. db is
// the write connection (DB.W), or any connection to the file in a test or
// a tool.
//
// Foreign keys are off meanwhile, as SQLite's procedure for schema
// changes asks: otherwise rebuilding a table would cascade the deletes of
// its old copy. foreign_key_check runs before each commit instead.
func Migrate(ctx context.Context, db *sql.DB, fsys fs.FS) (err error) {
	ms, err := migrations(fsys)
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var current int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return err
	}
	newest := 0
	if len(ms) > 0 {
		newest = ms[len(ms)-1].n
	}
	if current > newest {
		return fmt.Errorf("sqlite: the database is at version %d, newer than the newest migration (%d): refusing to run an older program on it", current, newest)
	}
	i := slices.IndexFunc(ms, func(m migration) bool { return m.n > current })
	if i < 0 {
		return nil
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	defer func() {
		if _, e := conn.ExecContext(context.Background(), "PRAGMA foreign_keys = ON"); e != nil {
			err = errors.Join(err, e)
		}
	}()
	for _, m := range ms[i:] {
		body, err := fs.ReadFile(fsys, m.name)
		if err != nil {
			return fmt.Errorf("sqlite: migration %s: %w", m.name, err)
		}
		if err := apply(ctx, conn, m.n, string(body)); err != nil {
			return fmt.Errorf("sqlite: migration %s: %w", m.name, err)
		}
	}
	return nil
}

func apply(ctx context.Context, conn *sql.Conn, n int, body string) error {
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
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(n)); err != nil {
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
