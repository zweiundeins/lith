package sqlite

import (
	"context"
	"database/sql"

	"github.com/zweiundeins/lith/internal/txctx"
)

// Querier is the method set for queries that *sql.DB, *sql.Tx and
// *sql.Conn share, and *writer.Tx as well: query code that takes a
// Querier serves a GET's View, a live Render and a command alike.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// WithTx returns a copy of ctx that carries tx. A View called with it, or
// with a context made from it, runs in tx when tx reads the same database
// file, so code that takes a ctx joins the caller's snapshot. A live
// Render's ctx carries its worker's transaction this way.
func WithTx(ctx context.Context, tx *sql.Tx) context.Context {
	return txctx.With(ctx, &txctx.Value{Tx: tx})
}

// TxFrom returns the transaction ctx carries (WithTx, or a live Render's).
// It does not tell which database the transaction reads; View checks that.
func TxFrom(ctx context.Context) (*sql.Tx, bool) {
	if v := txctx.From(ctx); v != nil && v.Tx != nil {
		return v.Tx, true
	}
	return nil, false
}

// QuerierFrom returns a Querier for the transaction ctx carries. In a live
// Render it runs each SQL text on a statement that the worker prepared
// once on its connection, in the render's transaction: modernc.org/sqlite
// parses and plans every query that has no prepared statement. The worker
// keeps the 512 statements used last; run SQL built at runtime, such as an
// IN list with a mark per value, with a ctx from Uncached, so that it
// takes no place in a cache and pushes none out. As with a command's Tx, scan a row
// before the same query runs again: until then its statement is busy.
// Elsewhere it is the transaction from WithTx. Like TxFrom, it does not
// tell which database the transaction reads.
func QuerierFrom(ctx context.Context) (Querier, bool) {
	v := txctx.From(ctx)
	switch {
	case v == nil:
		return nil, false
	case v.Querier != nil:
		return v.Querier, true
	case v.Tx != nil:
		return v.Tx, true
	}
	return nil, false
}

// Uncached returns a copy of ctx whose queries skip the statement caches:
// a query run with it on the read pool, on R, in a View's or a Render's
// tx, or through QuerierFrom in a Render, is prepared for this run and
// closed with its rows, and neither takes a place in a cache nor pushes
// one out. It is for SQL built at runtime, such as an IN list with a mark
// per value or a sort the request picks, which would take a place for
// every text it makes. A writer.Tx's ExecContext, QueryContext and
// QueryRowContext honour it as well, like its ExecUncached, QueryUncached
// and QueryRowUncached.
//
//	rows, err := q.QueryContext(sqlite.Uncached(ctx), `SELECT id, title FROM cards WHERE id IN (`+marks+`)`, ids...)
func Uncached(ctx context.Context) context.Context {
	return txctx.WithUncached(ctx)
}

// joined returns the transaction ctx carries if it reads d's file.
func (d *DB) joined(ctx context.Context) *sql.Tx {
	v := txctx.From(ctx)
	if v == nil || v.Tx == nil {
		return nil
	}
	if v.Pool != nil && v.Pool == d.R {
		return v.Tx
	}
	f, err := v.File(func() (string, error) { return mainFile(ctx, v.Tx) })
	if err == nil && f != "" && f == d.file {
		return v.Tx
	}
	return nil
}

// mainFile returns the file of the main database q reads, as SQLite names
// it: the same string for every connection opened with the same path.
func mainFile(ctx context.Context, q Querier) (string, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name string
		var file sql.NullString
		if err := rows.Scan(&seq, &name, &file); err != nil {
			return "", err
		}
		if name == "main" {
			return file.String, nil
		}
	}
	return "", rows.Err()
}
