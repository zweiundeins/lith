package board

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/zweiundeins/lith/sqlite"
)

// Store reads boards for code that takes a ctx rather than a transaction.
type Store struct{ DB *sqlite.DB }

// Board reads a board. Called from a live Render, or with a ctx that
// sqlite.WithTx made, it reads in that transaction, so it sees the
// caller's snapshot; with any other ctx it opens a read transaction of its
// own.
func (s Store) Board(ctx context.Context, id int64) (Board, error) {
	return s.DB.ViewValue(ctx, func(tx *sql.Tx) (Board, error) { return Load(ctx, tx, id) })
}

// Check opens a copy of the database for reading only and runs SQLite's
// integrity check on it: nothing is written, and a missing file is an
// error rather than a new, empty database.
func Check(ctx context.Context, path string) error {
	db, err := sqlite.Open(ctx, path, sqlite.Options{ReadOnly: true})
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.R.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("%s: %s", path, result)
	}
	return nil
}
