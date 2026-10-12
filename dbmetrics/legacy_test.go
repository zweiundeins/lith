package dbmetrics_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zweiundeins/lith/dbmetrics"
)

// The fakes have none of database/sql's optional interfaces unless a type
// embedding them adds one, so the wrapper has to take the fallbacks
// database/sql would take.

type fakeDriver struct{ conn func() driver.Conn }

func (d fakeDriver) Open(string) (driver.Conn, error) { return d.conn(), nil }

// seen records what the fakes were asked to do.
type seen struct {
	mu       sync.Mutex
	prepares int
	args     []driver.Value
}

func (s *seen) prepare() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prepares++
}

func (s *seen) record(args []driver.Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.args = append(s.args, args...)
}

type legacyConn struct{ seen *seen }

func (c legacyConn) Prepare(string) (driver.Stmt, error) {
	c.seen.prepare()
	return legacyStmt(c), nil
}
func (legacyConn) Close() error              { return nil }
func (legacyConn) Begin() (driver.Tx, error) { return legacyTx{}, nil }

type legacyTx struct{}

func (legacyTx) Commit() error   { return nil }
func (legacyTx) Rollback() error { return nil }

type legacyStmt struct{ seen *seen }

func (legacyStmt) Close() error  { return nil }
func (legacyStmt) NumInput() int { return -1 }
func (s legacyStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.seen.record(args)
	return driver.RowsAffected(len(args)), nil
}
func (s legacyStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.seen.record(args)
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

// execerConn adds the legacy one-shot Execer and Queryer.
type execerConn struct{ legacyConn }

func (c execerConn) Exec(_ string, args []driver.Value) (driver.Result, error) {
	return legacyStmt(c.legacyConn).Exec(args)
}
func (c execerConn) Query(_ string, args []driver.Value) (driver.Rows, error) {
	return legacyStmt(c.legacyConn).Query(args)
}

// checkingConn converts points, which the default converter refuses, and its
// statements convert the other arguments with a ColumnConverter.
type checkingConn struct{ legacyConn }

type point struct{ X, Y int }

func (checkingConn) CheckNamedValue(nv *driver.NamedValue) error {
	if p, ok := nv.Value.(point); ok {
		nv.Value = fmt.Sprintf("%d,%d", p.X, p.Y)
		return nil
	}
	return driver.ErrSkip
}

func (c checkingConn) Prepare(string) (driver.Stmt, error) {
	c.seen.prepare()
	return convertingStmt{legacyStmt(c.legacyConn)}, nil
}

type convertingStmt struct{ legacyStmt }

func (convertingStmt) ColumnConverter(int) driver.ValueConverter { return upper{} }

type upper struct{}

func (upper) ConvertValue(v any) (driver.Value, error) {
	if s, ok := v.(string); ok {
		return strings.ToUpper(s), nil
	}
	return driver.DefaultParameterConverter.ConvertValue(v)
}

func openFake(t *testing.T, conn func() driver.Conn, wrap bool, opts dbmetrics.Options) *sql.DB {
	t.Helper()
	var d driver.Driver = fakeDriver{conn}
	if wrap {
		d = dbmetrics.Wrap(d, opts)
	}
	name := fmt.Sprintf("fake-%d", registrations.Add(1))
	sql.Register(name, d)
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func readAll(rows *sql.Rows, err error) ([]int64, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func TestADriverWithoutContextMethods(t *testing.T) {
	rec := &recorder{}
	s := &seen{}
	db := openFake(t, func() driver.Conn { return legacyConn{s} }, true, dbmetrics.Options{Observer: rec})

	res, err := db.Exec(`INSERT`, 1, 2)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Errorf("RowsAffected = %d, want 2", n)
	}
	if got, err := readAll(db.Query(`SELECT`, int64(7), int64(8))); err != nil || !reflect.DeepEqual(got, []int64{7, 8}) {
		t.Errorf("rows = %v, %v; want 7, 8", got, err)
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

func TestTheLegacyExecerSkipsPrepare(t *testing.T) {
	rec := &recorder{}
	s := &seen{}
	db := openFake(t, func() driver.Conn { return execerConn{legacyConn{s}} }, true, dbmetrics.Options{Observer: rec})

	if _, err := db.Exec(`INSERT`, 1); err != nil {
		t.Fatal(err)
	}
	if got, err := readAll(db.Query(`SELECT`, int64(7))); err != nil || !reflect.DeepEqual(got, []int64{7}) {
		t.Errorf("rows = %v, %v; want 7", got, err)
	}
	if s.prepares != 0 {
		t.Errorf("prepared %d statements, want the Execer and Queryer used directly", s.prepares)
	}
	if obs := rec.take(); len(obs) != 2 {
		t.Errorf("observations = %+v, want one exec and one query", obs)
	}
}

// TestArgumentsAreConvertedAsTheDriverWould runs one statement through the
// driver alone and through the wrapper, and compares what the driver got.
func TestArgumentsAreConvertedAsTheDriverWould(t *testing.T) {
	var got [2][]driver.Value
	for i, wrap := range []bool{false, true} {
		s := &seen{}
		db := openFake(t, func() driver.Conn { return checkingConn{legacyConn{s}} }, wrap, dbmetrics.Options{})
		if _, err := db.Exec(`INSERT`, point{1, 2}, "abc", int64(3)); err != nil {
			t.Fatalf("Exec (wrapped %v): %v", wrap, err)
		}
		got[i] = s.args
	}
	want := []driver.Value{"1,2", "ABC", int64(3)}
	if !reflect.DeepEqual(got[0], want) || !reflect.DeepEqual(got[1], want) {
		t.Errorf("the driver got %v alone and %v wrapped, want %v both times", got[0], got[1], want)
	}
}
