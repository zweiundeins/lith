package dbmetrics_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/zweiundeins/lith/dbmetrics"
)

func init() {
	// sleep_ms(n) makes a statement slow on demand, row by row.
	sqlite.MustRegisterScalarFunction("sleep_ms", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		time.Sleep(time.Duration(args[0].(int64)) * time.Millisecond)
		return args[0], nil
	})
}

type observation struct {
	op      string
	elapsed time.Duration
	slow    bool
}

type recorder struct {
	mu  sync.Mutex
	obs []observation
}

func (r *recorder) Observe(op string, elapsed time.Duration, slow bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.obs = append(r.obs, observation{op, elapsed, slow})
}

func (r *recorder) take() []observation {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.obs
	r.obs = nil
	return out
}

func (r *recorder) count(op string) int {
	n := 0
	for _, o := range r.take() {
		if o.op == op {
			n++
		}
	}
	return n
}

func open(t *testing.T, opts dbmetrics.Options) *sql.DB {
	t.Helper()
	c, err := sqlite.NewConnector("file:" + filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(dbmetrics.WrapConnector(c, opts))
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func TestStatementsAreTimedAndResultsUntouched(t *testing.T) {
	rec := &recorder{}
	db := open(t, dbmetrics.Options{Observer: rec})

	mustExec(t, db, `CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)`)
	mustExec(t, db, `INSERT INTO t (id, name) VALUES (?, ?), (?, ?)`, 1, "pug", 2, "beagle")
	if n := rec.count(dbmetrics.OpExec); n != 2 {
		t.Errorf("exec observations = %d, want 2", n)
	}

	var name string
	if err := db.QueryRow(`SELECT name FROM t WHERE id = ?`, 1).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "pug" {
		t.Errorf("name = %q, want pug", name)
	}
	rows, err := db.Query(`SELECT id, name FROM t ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var id int
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if strings.Join(got, ",") != "pug,beagle" {
		t.Errorf("rows = %v, want pug,beagle", got)
	}
	if n := rec.count(dbmetrics.OpQuery); n != 2 {
		t.Errorf("query observations = %d, want one per query", n)
	}
}

func TestPreparedStatementsAndTransactionsAreTimed(t *testing.T) {
	rec := &recorder{}
	db := open(t, dbmetrics.Options{Observer: rec})
	mustExec(t, db, `CREATE TABLE t (id INTEGER)`)
	rec.take()

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	st, err := tx.Prepare(`INSERT INTO t (id) VALUES (?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := st.Exec(i); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := rec.count(dbmetrics.OpExec); n != 3 {
		t.Errorf("exec observations = %d, want 3", n)
	}

	q, err := db.Prepare(`SELECT count(*) FROM t WHERE id >= ?`)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	var n int
	if err := q.QueryRow(1).Scan(&n); err != nil || n != 2 {
		t.Fatalf("count = %d, %v; want 2", n, err)
	}
	if got := rec.count(dbmetrics.OpQuery); got != 1 {
		t.Errorf("query observations = %d, want 1", got)
	}
}

// TestQueryTimeIncludesNext: SQLite computes rows as they are stepped, so a
// query's time must include every Next, not only the call that started it.
func TestQueryTimeIncludesNext(t *testing.T) {
	rec := &recorder{}
	db := open(t, dbmetrics.Options{Observer: rec})

	rows, err := db.Query(`SELECT sleep_ms(15) FROM (VALUES (1), (2), (3), (4))`)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	_ = rows.Close()
	if n != 4 {
		t.Fatalf("rows = %d, want 4", n)
	}
	obs := rec.take()
	if len(obs) != 1 {
		t.Fatalf("observations = %v, want one", obs)
	}
	if obs[0].elapsed < 60*time.Millisecond {
		t.Errorf("elapsed = %v, want at least the 60ms spent stepping four rows", obs[0].elapsed)
	}
}

func TestSlowStatementsAreLoggedWithoutArguments(t *testing.T) {
	rec := &recorder{}
	var buf bytes.Buffer
	db := open(t, dbmetrics.Options{
		Observer:      rec,
		SlowThreshold: 10 * time.Millisecond,
		Logger:        slog.New(slog.NewTextHandler(&buf, nil)),
	})

	var v any
	if err := db.QueryRow(`SELECT sleep_ms(20),
		? AS secret`, "s3cret").Scan(&v, new(string)); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `SELECT 1`) // no write, so no fsync to make it slow

	obs := rec.take()
	if len(obs) != 2 || !obs[0].slow || obs[1].slow {
		t.Fatalf("observations = %+v, want the query slow and the exec not", obs)
	}
	log := buf.String()
	if strings.Count(log, "slow db query") != 1 {
		t.Fatalf("log = %q, want one slow line", log)
	}
	if !strings.Contains(log, "op=query") || !strings.Contains(log, "SELECT sleep_ms(20), ? AS secret") {
		t.Errorf("log = %q, want the op and the query on one line", log)
	}
	if strings.Contains(log, "s3cret") {
		t.Errorf("log = %q, it must not contain arguments", log)
	}
}

func TestANegativeThresholdTurnsTheSlowLogOff(t *testing.T) {
	rec := &recorder{}
	var buf bytes.Buffer
	db := open(t, dbmetrics.Options{
		Observer:      rec,
		SlowThreshold: -1,
		Logger:        slog.New(slog.NewTextHandler(&buf, nil)),
	})
	var v int64
	if err := db.QueryRow(`SELECT sleep_ms(5)`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if obs := rec.take(); len(obs) != 1 || obs[0].slow {
		t.Errorf("observations = %+v, want one, not slow", obs)
	}
	if buf.Len() != 0 {
		t.Errorf("log = %q, want nothing", buf.String())
	}
}

func TestAFailedStatementIsRecorded(t *testing.T) {
	rec := &recorder{}
	db := open(t, dbmetrics.Options{Observer: rec})
	if _, err := db.Query(`SELECT * FROM missing`); err == nil {
		t.Fatal("query on a missing table succeeded")
	}
	if _, err := db.Exec(`INSERT INTO missing VALUES (1)`); err == nil {
		t.Fatal("insert into a missing table succeeded")
	}
	obs := rec.take()
	if len(obs) != 2 || obs[0].op != dbmetrics.OpQuery || obs[1].op != dbmetrics.OpExec {
		t.Errorf("observations = %+v, want the failed query and exec", obs)
	}
}

func TestColumnTypesPassThrough(t *testing.T) {
	db := open(t, dbmetrics.Options{})
	mustExec(t, db, `CREATE TABLE t (id INTEGER, name TEXT, score REAL)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 'a', 1.5)`)
	rows, err := db.Query(`SELECT id, name, score FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	types, err := rows.ColumnTypes()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ct := range types {
		got = append(got, ct.DatabaseTypeName())
	}
	if strings.Join(got, ",") != "INTEGER,TEXT,REAL" {
		t.Errorf("database types = %v, want the driver's", got)
	}
}

func TestUnwrapReachesTheDriverConnection(t *testing.T) {
	db := open(t, dbmetrics.Options{})
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	err = conn.Raw(func(dc any) error {
		u, ok := dc.(interface{ Unwrap() driver.Conn })
		if !ok {
			t.Fatal("the wrapped connection has no Unwrap")
		}
		if _, ok := u.Unwrap().(sqlite.FileControl); !ok {
			t.Error("Unwrap did not return the modernc connection")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// registrations keeps driver names unique when the test runs more than once
// in one process: sql.Register cannot be undone.
var registrations atomic.Int32

func TestRegister(t *testing.T) {
	rec := &recorder{}
	name := fmt.Sprintf("sqlite-dbmetrics-%d", registrations.Add(1))
	if err := dbmetrics.Register(name, "sqlite", dbmetrics.Options{Observer: rec}); err != nil {
		t.Fatal(err)
	}
	if err := dbmetrics.Register(name, "sqlite", dbmetrics.Options{}); err == nil {
		t.Error("registering a taken name succeeded")
	}
	if err := dbmetrics.Register(name+"-x", "no-such-driver", dbmetrics.Options{}); err == nil {
		t.Error("registering over an unknown driver succeeded")
	}

	db, err := sql.Open(name, filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, `CREATE TABLE t (id INTEGER)`)
	if n := rec.count(dbmetrics.OpExec); n != 1 {
		t.Errorf("exec observations through the registered driver = %d, want 1", n)
	}
}
