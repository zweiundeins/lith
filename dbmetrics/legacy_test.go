package dbmetrics_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/zweiundeins/lith/dbmetrics"
)

// legacyDriver has none of the optional interfaces, so the wrapper has to
// take the fallbacks database/sql would take.
type legacyDriver struct{}

func (legacyDriver) Open(string) (driver.Conn, error) { return legacyConn{}, nil }

type legacyConn struct{}

func (legacyConn) Prepare(q string) (driver.Stmt, error) { return legacyStmt{}, nil }
func (legacyConn) Close() error                          { return nil }
func (legacyConn) Begin() (driver.Tx, error)             { return legacyTx{}, nil }

type legacyTx struct{}

func (legacyTx) Commit() error   { return nil }
func (legacyTx) Rollback() error { return nil }

type legacyStmt struct{}

func (legacyStmt) Close() error  { return nil }
func (legacyStmt) NumInput() int { return -1 }
func (legacyStmt) Exec(args []driver.Value) (driver.Result, error) {
	return driver.RowsAffected(len(args)), nil
}
func (legacyStmt) Query(args []driver.Value) (driver.Rows, error) {
	return &legacyRows{vals: args}, nil
}

// legacyRows returns the query's arguments as its rows.
type legacyRows struct {
	vals []driver.Value
	next int
}

func (*legacyRows) Columns() []string { return []string{"v"} }
func (*legacyRows) Close() error      { return nil }
func (r *legacyRows) Next(dest []driver.Value) error {
	if r.next == len(r.vals) {
		return io.EOF
	}
	dest[0] = r.vals[r.next]
	r.next++
	return nil
}

func TestADriverWithoutContextMethods(t *testing.T) {
	rec := &recorder{}
	name := fmt.Sprintf("legacy-%d", registrations.Add(1))
	sql.Register(name, dbmetrics.Wrap(legacyDriver{}, dbmetrics.Options{Observer: rec}))
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	res, err := db.Exec(`INSERT`, 1, 2)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Errorf("RowsAffected = %d, want 2", n)
	}
	rows, err := db.Query(`SELECT`, int64(7), int64(8))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var got []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	_ = rows.Close()
	if len(got) != 2 || got[0] != 7 || got[1] != 8 {
		t.Errorf("rows = %v, want 7, 8", got)
	}
	obs := rec.take()
	if len(obs) != 2 || obs[0].op != dbmetrics.OpExec || obs[1].op != dbmetrics.OpQuery {
		t.Errorf("observations = %+v, want one exec and one query", obs)
	}

	if _, err := db.Exec(`INSERT`, sql.Named("a", 1)); err == nil || !strings.Contains(err.Error(), "Named Parameters") {
		t.Errorf("Exec with a named argument = %v, want database/sql's refusal", err)
	}
	if _, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true}); err == nil {
		t.Error("a read-only transaction on a driver without BeginTx succeeded")
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
