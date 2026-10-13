package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	msqlite "modernc.org/sqlite"

	"github.com/zweiundeins/lith/dbmetrics"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"
)

// counts are the statements prepared on a driver's connections and the
// ones closed, in all and by SQL text, and the ones closed a second time.
type counts struct {
	mu                      sync.Mutex
	prepares, closes        map[string]int
	prepared, closed, twice int
}

func (n *counts) of(query string) (prepared, closed int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.prepares[query], n.closes[query]
}

func (n *counts) totals() (prepared, closed int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.prepared, n.closed
}

// check fails t unless every statement was closed, and each once. Call it
// once the database is closed.
func (n *counts) check(t *testing.T) {
	t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.prepared != n.closed || n.twice != 0 {
		t.Fatalf("%d statements prepared, %d closed, %d closed twice", n.prepared, n.closed, n.twice)
	}
}

// countingDriver registers modernc's driver wrapped so that it counts its
// statements, for Options.Driver.
func countingDriver(t *testing.T) (string, *counts) {
	t.Helper()
	db, err := sql.Open("sqlite", "")
	if err != nil {
		t.Fatal(err)
	}
	base := db.Driver()
	db.Close()
	n := &counts{prepares: map[string]int{}, closes: map[string]int{}}
	name := fmt.Sprintf("sqlite-counting-%d", registrations.Add(1))
	sql.Register(name, countDriver{base, n})
	return name, n
}

type countDriver struct {
	driver.Driver
	n *counts
}

func (d countDriver) Open(dsn string) (driver.Conn, error) {
	c, err := d.Driver.Open(dsn)
	if err != nil {
		return nil, err
	}
	return countConn{c.(ctxConn), d.n}, nil
}

type countConn struct {
	ctxConn
	n *counts
}

func (c countConn) Unwrap() driver.Conn { return c.ctxConn }

func (c countConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	s, err := c.ctxConn.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	c.n.mu.Lock()
	c.n.prepared++
	c.n.prepares[query]++
	c.n.mu.Unlock()
	return &countStmt{Stmt: s, n: c.n, query: query}, nil
}

type countStmt struct {
	driver.Stmt
	n      *counts
	query  string
	closed bool
}

func (s *countStmt) Close() error {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	if s.closed {
		s.n.twice++
		return nil
	}
	s.closed = true
	s.n.closed++
	s.n.closes[s.query]++
	return s.Stmt.Close()
}

func (s *countStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.Stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
}

func (s *countStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.Stmt.(driver.StmtExecContext).ExecContext(ctx, args)
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// ints reads rows of one integer each and closes them.
func ints(rows *sql.Rows, err error) ([]int, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var x int
		if err := rows.Scan(&x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// cached returns the SQL texts c keeps statements for, the one used last
// first, but for the PRAGMA that Open runs.
func cached(t *testing.T, c *sql.Conn) []string {
	t.Helper()
	var out []string
	err := c.Raw(func(dc any) error {
		sc, ok := dc.(*stmtConn)
		if !ok {
			return fmt.Errorf("%T keeps no statements", dc)
		}
		n := 0
		for s := sc.lru.front; s != nil; s = s.next {
			if n++; s.query != "PRAGMA database_list" {
				out = append(out, s.query)
			}
		}
		if n != len(sc.stmts) {
			return fmt.Errorf("%d statements listed, %d in the map", n, len(sc.stmts))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// stmtBytes is the memory of the statements prepared on c, read from the
// driver's connection that c Unwraps to.
func stmtBytes(t testing.TB, c *sql.Conn) int {
	t.Helper()
	var n int
	err := c.Raw(func(dc any) error {
		for {
			if s, ok := dc.(msqlite.DBStatus); ok {
				var err error
				n, _, err = s.Status(msqlite.DBStatusStmtUsed, false)
				return err
			}
			u, ok := dc.(interface{ Unwrap() driver.Conn })
			if !ok {
				return fmt.Errorf("%T has no DBStatus", dc)
			}
			dc = u.Unwrap()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Each SQL text is prepared once per read connection: in a View, outside
// one, and in the transactions after.
func TestStatementsPreparedOnce(t *testing.T) {
	ctx := context.Background()
	name, n := countingDriver(t)
	d := open(t, Options{Driver: name, Readers: 1})
	exec(t, d.W, "CREATE TABLE t (x INTEGER); INSERT INTO t VALUES (1), (2), (3)")
	const sum, count = "SELECT sum(x) FROM t WHERE x > ?", "SELECT count(*) FROM t"
	for i := range 20 {
		var s, c int
		err := d.View(ctx, func(tx *sql.Tx) error {
			if err := tx.QueryRowContext(ctx, sum, i%3).Scan(&s); err != nil {
				return err
			}
			return tx.QueryRowContext(ctx, count).Scan(&c)
		})
		if err != nil {
			t.Fatal(err)
		}
		if want := []int{6, 5, 3}[i%3]; s != want || c != 3 {
			t.Fatalf("round %d: sum %d, count %d", i, s, c)
		}
		if err := d.R.QueryRowContext(ctx, count).Scan(&c); err != nil || c != 3 {
			t.Fatalf("count %d: %v", c, err)
		}
	}
	if s, _ := n.of(sum); s != 1 {
		t.Fatalf("%q prepared %d times, want once", sum, s)
	}
	if c, _ := n.of(count); c != 1 {
		t.Fatalf("%q prepared %d times, want once", count, c)
	}
	d.Close()
	n.check(t)
}

// A query that runs again while its statement's rows are open runs on a
// statement of its own: nested in the rows of the same query, or with the
// row of an earlier QueryRow not scanned yet.
func TestBusyStatementsRunOnTheirOwn(t *testing.T) {
	ctx := context.Background()
	name, n := countingDriver(t)
	d := open(t, Options{Driver: name, Readers: 1})
	exec(t, d.W, "CREATE TABLE t (x INTEGER); INSERT INTO t VALUES (1), (2), (3)")
	const all, tenfold = "SELECT x FROM t ORDER BY x", "SELECT x * 10 FROM t WHERE x = ?"
	for range 2 {
		var pairs [][2]int
		err := d.View(ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, all)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var x int
				if err := rows.Scan(&x); err != nil {
					return err
				}
				if inner, err := ints(tx.QueryContext(ctx, all)); err != nil || !slices.Equal(inner, []int{1, 2, 3}) {
					return fmt.Errorf("nested: %v, %v", inner, err)
				}
				first, second := tx.QueryRowContext(ctx, tenfold, x), tx.QueryRowContext(ctx, tenfold, 4-x)
				var a, b int
				if err := second.Scan(&b); err != nil {
					return err
				}
				if err := first.Scan(&a); err != nil {
					return err
				}
				pairs = append(pairs, [2]int{a, b})
			}
			return rows.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		if want := [][2]int{{10, 30}, {20, 20}, {30, 10}}; !slices.Equal(pairs, want) {
			t.Fatalf("read %v, want %v", pairs, want)
		}
	}
	a, _ := n.of(all)
	b, _ := n.of(tenfold)
	if a != 1 || b != 1 {
		t.Fatalf("prepared %d and %d times: the runs on statements of their own took a place", a, b)
	}
	d.Close()
	n.check(t)
}

// The cache keeps the statements used last and closes the ones it pushes
// out; one pushed out while its rows are open is closed with them.
func TestStatementCacheKeepsTheLastUsed(t *testing.T) {
	ctx := context.Background()
	name, n := countingDriver(t)
	d := open(t, Options{Driver: name, Readers: 1, Statements: 2})
	exec(t, d.W, "CREATE TABLE t (x INTEGER); INSERT INTO t VALUES (1), (2), (3)")
	q := func(i int) string { return fmt.Sprintf("SELECT x + %d FROM t ORDER BY x", i) }
	read := func(i int) {
		t.Helper()
		got, err := ints(d.R.QueryContext(ctx, q(i)))
		if want := []int{1 + i, 2 + i, 3 + i}; err != nil || !slices.Equal(got, want) {
			t.Fatalf("%s read %v, %v", q(i), got, err)
		}
	}
	read(1)
	read(2)
	read(1)
	read(3)
	c, err := d.R.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cached(t, c), []string{q(3), q(1)}; !slices.Equal(got, want) {
		t.Fatalf("cached %q, want %q", got, want)
	}
	c.Close()
	for i, want := range []int{0, 1, 0} {
		if p, c := n.of(q(i + 1)); p != 1 || c != want {
			t.Fatalf("%s prepared %d times, closed %d, want 1 and %d", q(i+1), p, c, want)
		}
	}

	err = d.View(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, q(1))
		if err != nil {
			return err
		}
		defer rows.Close()
		var got []int
		for rows.Next() {
			var x int
			if err := rows.Scan(&x); err != nil {
				return err
			}
			got = append(got, x)
			if len(got) == 1 {
				// Two more texts push out q(3), then q(1), whose rows are open.
				for _, i := range []int{4, 5} {
					if err := tx.QueryRowContext(ctx, q(i)).Scan(new(int)); err != nil {
						return err
					}
				}
				_, c3 := n.of(q(3))
				_, c1 := n.of(q(1))
				if c3 != 1 || c1 != 0 {
					return fmt.Errorf("closed %d and %d times, want once and not yet", c3, c1)
				}
			}
		}
		if !slices.Equal(got, []int{2, 3, 4}) {
			return fmt.Errorf("read %v from a statement pushed out", got)
		}
		rows.Close()
		if _, c := n.of(q(1)); c != 1 {
			return fmt.Errorf("closed %d times after its rows, want once", c)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}

	c, err = d.R.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cached(t, c), []string{q(5), q(4)}; !slices.Equal(got, want) {
		t.Fatalf("cached %q, want %q", got, want)
	}
	full := stmtBytes(t, c)
	c.Close()
	for i := 6; i < 200; i++ {
		read(i)
	}
	c, err = d.R.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if b := stmtBytes(t, c); b > full*5/4 {
		t.Errorf("statements hold %d bytes after 194 more texts, %d before", b, full)
	}
	c.Close()
	d.Close()
	n.check(t)
}

// SQLite prepares a statement again when the schema changed since it was
// prepared: a migration or an ALTER between reads shows in the next read.
func TestStatementsFollowTheSchema(t *testing.T) {
	ctx := context.Background()
	name, n := countingDriver(t)
	d := open(t, Options{Driver: name, Readers: 1})
	exec(t, d.W, "CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT NOT NULL); INSERT INTO notes (body) VALUES ('a')")
	const all, body = "SELECT * FROM notes ORDER BY id", "SELECT body FROM notes ORDER BY id"
	read := func() string {
		t.Helper()
		rows, err := d.R.QueryContext(ctx, all)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		var out []string
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, c := range cols {
				out = append(out, fmt.Sprintf("%s=%v", c, vals[i]))
			}
		}
		return strings.Join(out, " ")
	}
	if got := read(); got != "id=1 body=a" {
		t.Fatalf("read %q", got)
	}
	err := Migrate(ctx, d.W, files(map[string]string{"001_tag.sql": "ALTER TABLE notes ADD COLUMN tag TEXT NOT NULL DEFAULT 'x'"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "id=1 body=a tag=x" {
		t.Fatalf("after the migration: %q", got)
	}
	exec(t, d.W, "ALTER TABLE notes DROP COLUMN body")
	if got := read(); got != "id=1 tag=x" {
		t.Fatalf("after DROP COLUMN: %q", got)
	}
	if _, err := d.R.QueryContext(ctx, body); err == nil || !strings.Contains(err.Error(), "no such column") {
		t.Fatalf("a column dropped: %v", err)
	}
	exec(t, d.W, "ALTER TABLE notes ADD COLUMN body TEXT NOT NULL DEFAULT 'b'")
	var b string
	if err := d.R.QueryRowContext(ctx, body).Scan(&b); err != nil || b != "b" {
		t.Fatalf("the column back: %q, %v", b, err)
	}
	exec(t, d.W, "DROP TABLE notes; CREATE TABLE notes (id INTEGER PRIMARY KEY, n INTEGER); INSERT INTO notes (n) VALUES (7)")
	if got := read(); got != "id=1 n=7" {
		t.Fatalf("a new table: %q", got)
	}
	if p, _ := n.of(all); p != 1 {
		t.Fatalf("%q prepared %d times, want once: SQLite prepares it again itself", all, p)
	}
	d.Close()
	n.check(t)
}

// SQL that does not prepare, or does not bind, fails as it would without
// the cache, keeps no statement, and runs once it can.
func TestStatementErrors(t *testing.T) {
	ctx := context.Background()
	name, n := countingDriver(t)
	d := open(t, Options{Driver: name, Readers: 1})
	plain := open(t, Options{Statements: -1, Readers: 1})
	for _, db := range []*DB{d, plain} {
		exec(t, db.W, "CREATE TABLE t (x INTEGER); INSERT INTO t VALUES (1)")
	}
	const missing = "SELECT x FROM later"
	for _, q := range []string{"SELEC 1", missing, "SELECT x FROM t WHERE x = ? AND x = ?"} {
		for range 2 {
			_, err := d.R.QueryContext(ctx, q)
			_, want := plain.R.QueryContext(ctx, q)
			if err == nil || want == nil || err.Error() != want.Error() {
				t.Fatalf("%s: %v, want %v", q, err, want)
			}
		}
	}
	if got, err := ints(d.R.QueryContext(ctx, "SELECT x FROM t WHERE x = ? AND x = ?", 1, 1)); err != nil || !slices.Equal(got, []int{1}) {
		t.Fatalf("after missing arguments: %v, %v", got, err)
	}
	exec(t, d.W, "CREATE TABLE later (x INTEGER); INSERT INTO later VALUES (7)")
	if got, err := ints(d.R.QueryContext(ctx, missing)); err != nil || !slices.Equal(got, []int{7}) {
		t.Fatalf("once the table is there: %v, %v", got, err)
	}
	c, err := d.R.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cached(t, c), []string{missing, "SELECT x FROM t WHERE x = ? AND x = ?"}; !slices.Equal(got, want) {
		t.Fatalf("cached %q, want %q", got, want)
	}
	c.Close()
	d.Close()
	n.check(t)
}

// A query whose context ends leaves its statement to the next query; one
// that SQLite interrupts costs at most its connection.
func TestContextEndsAQuery(t *testing.T) {
	ctx := context.Background()
	name, n := countingDriver(t)
	d := open(t, Options{Driver: name, Readers: 1})
	const series = "WITH RECURSIVE s(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM s WHERE x < ?) SELECT x FROM s"
	const count = "WITH RECURSIVE s(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM s WHERE x < ?) SELECT count(*) FROM s"

	qctx, cancel := context.WithCancel(ctx)
	rows, err := d.R.QueryContext(qctx, series, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		rows.Next()
	}
	cancel()
	for rows.Next() {
	}
	if err := rows.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("rows of a cancelled query: %v", err)
	}
	if got, err := ints(d.R.QueryContext(ctx, series, 4)); err != nil || !slices.Equal(got, []int{1, 2, 3, 4}) {
		t.Fatalf("after a cancelled query: %v, %v", got, err)
	}

	var c int
	for _, in := range []bool{false, true} {
		if err := d.R.QueryRowContext(ctx, count, 10).Scan(&c); err != nil || c != 10 {
			t.Fatalf("count %d: %v", c, err)
		}
		qctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		if in {
			err = d.View(qctx, func(tx *sql.Tx) error {
				return tx.QueryRowContext(qctx, count, 1<<40).Scan(&c)
			})
		} else {
			err = d.R.QueryRowContext(qctx, count, 1<<40).Scan(&c)
		}
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("an interrupted query: %v", err)
		}
		if err := d.R.QueryRowContext(ctx, count, 10).Scan(&c); err != nil || c != 10 {
			t.Fatalf("after an interrupted query: count %d, %v", c, err)
		}
	}
	if p, _ := n.of(series); p != 1 {
		t.Fatalf("%d prepares after a cancelled query, want 1", p)
	}
	d.Close()
	n.check(t)
}

// A read-only database's connections keep their statements too.
func TestReadOnlyConnectionsKeepStatements(t *testing.T) {
	ctx := context.Background()
	rw := open(t, Options{})
	exec(t, rw.W, "CREATE TABLE t (x INTEGER); INSERT INTO t VALUES (1), (2)")
	name, n := countingDriver(t)
	d, err := Open(ctx, rw.Path, Options{ReadOnly: true, Readers: 1, Driver: name})
	if err != nil {
		t.Fatal(err)
	}
	const count = "SELECT count(*) FROM t"
	for range 5 {
		if got := scalar(t, d, count); got != 2 {
			t.Fatalf("count %d", got)
		}
	}
	if p, _ := n.of(count); p != 1 {
		t.Fatalf("prepared %d times", p)
	}
	if _, err := d.R.ExecContext(ctx, "INSERT INTO t VALUES (3)"); err == nil {
		t.Fatal("wrote through a read-only database")
	}
	d.Close()
	n.check(t)
}

// SQLite carries out a PRAGMA as it prepares it, and marks it to be
// prepared again before it runs again: one that runs again on a read
// connection takes effect again, from the cache or not.
func TestPragmasTakeEffectAgain(t *testing.T) {
	ctx := context.Background()
	d := open(t, Options{Readers: 1})
	c, err := d.R.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, ms := range []string{"0", "250", "0", "250"} {
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout = "+ms).Scan(new(int)); err != nil {
			t.Fatal(err)
		}
		if got := pragma(t, c, "busy_timeout"); got != ms {
			t.Fatalf("busy_timeout %s, want %s", got, ms)
		}
	}
}

type queryCounter struct{ queries atomic.Int64 }

func (o *queryCounter) Observe(op string, _ time.Duration, _ bool) {
	if op == dbmetrics.OpQuery {
		o.queries.Add(1)
	}
}

// dbmetrics' Observer sees every query on the read pool: those that run
// on a cached statement, those that run on one of their own, and those
// that fail.
func TestObserverSeesEveryQuery(t *testing.T) {
	ctx := context.Background()
	var obs queryCounter
	name := fmt.Sprintf("sqlite-observed-%d", registrations.Add(1))
	if err := dbmetrics.Register(name, "sqlite", dbmetrics.Options{Observer: &obs, SlowThreshold: -1}); err != nil {
		t.Fatal(err)
	}
	d := open(t, Options{Driver: name, Readers: 1})
	exec(t, d.W, "CREATE TABLE t (x INTEGER); INSERT INTO t VALUES (1), (2)")
	const all = "SELECT x FROM t ORDER BY x"
	before := obs.queries.Load()
	queries := 0
	err := d.View(ctx, func(tx *sql.Tx) error {
		for range 3 {
			rows, err := tx.QueryContext(ctx, all)
			if err != nil {
				return err
			}
			queries++
			for rows.Next() {
				if err := tx.QueryRowContext(ctx, all).Scan(new(int)); err != nil {
					return err
				}
				queries++
			}
			rows.Close()
		}
		if _, err := tx.QueryContext(ctx, "SELECT nope FROM t"); err == nil {
			return errors.New("no error")
		}
		queries++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := obs.queries.Load() - before; got != int64(queries) {
		t.Fatalf("the Observer saw %d queries of %d", got, queries)
	}
	c, err := d.R.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := cached(t, c); !slices.Equal(got, []string{all}) {
		t.Fatalf("cached %q", got)
	}
}

// Statements defaults to 128; negative, the read pool's connections are
// the driver's.
func TestStatementsOff(t *testing.T) {
	ctx := context.Background()
	if got := open(t, Options{}).Options().Statements; got != 128 {
		t.Errorf("Statements defaults to %d", got)
	}
	name, n := countingDriver(t)
	d := open(t, Options{Driver: name, Readers: 1, Statements: -1})
	for range 3 {
		if got := scalar(t, d, "SELECT 1"); got != 1 {
			t.Fatal(got)
		}
	}
	c, err := d.R.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.Raw(func(dc any) error {
		if _, ok := dc.(*stmtConn); ok {
			return errors.New("the connection keeps statements")
		}
		return nil
	})
	if p, _ := n.totals(); err != nil || p != 0 {
		t.Fatalf("%v, %d statements prepared", err, p)
	}
}

// The rows of a cached statement report the driver's column types.
func TestColumnTypes(t *testing.T) {
	ctx := context.Background()
	types := func(d *DB) string {
		exec(t, d.W, "CREATE TABLE t (i INTEGER NOT NULL, s TEXT, f REAL, b BLOB); INSERT INTO t VALUES (1, 'a', 1.5, x'00')")
		var out []string
		for range 2 {
			rows, err := d.R.QueryContext(ctx, "SELECT i, s, f, b FROM t")
			if err != nil {
				t.Fatal(err)
			}
			cts, err := rows.ColumnTypes()
			if err != nil {
				t.Fatal(err)
			}
			for _, ct := range cts {
				nullable, ok := ct.Nullable()
				length, lok := ct.Length()
				out = append(out, fmt.Sprint(ct.DatabaseTypeName(), ct.ScanType(), nullable, ok, length, lok))
			}
			rows.Close()
		}
		return strings.Join(out, "; ")
	}
	if got, want := types(open(t, Options{})), types(open(t, Options{Statements: -1})); got != want {
		t.Fatalf("column types %s, want %s", got, want)
	}
}

// Many goroutines read through a small pool with a small cache, nested,
// with rows in flight, some until their context ends, while the write
// connection commits.
func TestStatementCacheUnderLoad(t *testing.T) {
	ctx := context.Background()
	name, n := countingDriver(t)
	d := open(t, Options{Driver: name, Readers: 4, Statements: 3})
	exec(t, d.W, "CREATE TABLE t (id INTEGER PRIMARY KEY, x INTEGER NOT NULL)")
	exec(t, d.W, "INSERT INTO t VALUES (1, 1)")
	const (
		count  = "SELECT count(*) FROM t"
		last   = "SELECT x FROM t ORDER BY id DESC LIMIT 5"
		byID   = "SELECT x FROM t WHERE id = ?"
		bigger = "SELECT count(*) FROM t WHERE id >= ?"
	)
	stop := make(chan struct{})
	var writes sync.WaitGroup
	writes.Go(func() {
		for i := 2; i <= 3000; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := d.W.ExecContext(ctx, "INSERT INTO t VALUES (?, ?)", i, i); err != nil {
				t.Error(err)
				return
			}
		}
	})
	var reads sync.WaitGroup
	for g := range 16 {
		reads.Go(func() {
			for i := range 100 {
				qctx, cancel := ctx, context.CancelFunc(func() {})
				if (g+i)%7 == 0 {
					qctx, cancel = context.WithTimeout(ctx, time.Duration(i%4)*50*time.Microsecond)
				}
				err := d.View(qctx, func(tx *sql.Tx) error {
					var before, after int
					if err := tx.QueryRowContext(qctx, count).Scan(&before); err != nil {
						return err
					}
					rows, err := tx.QueryContext(qctx, last)
					if err != nil {
						return err
					}
					defer rows.Close()
					for rows.Next() {
						var x, y, z int
						if err := rows.Scan(&x); err != nil {
							return err
						}
						first := tx.QueryRowContext(qctx, byID, x)
						if err := tx.QueryRowContext(qctx, bigger, x).Scan(&z); err != nil {
							return err
						}
						if err := first.Scan(&y); err != nil {
							return err
						}
						if y != x || z != before-x+1 {
							return fmt.Errorf("x %d: read %d and %d of %d", x, y, z, before)
						}
					}
					if err := rows.Err(); err != nil {
						return err
					}
					if err := tx.QueryRowContext(qctx, count).Scan(&after); err != nil {
						return err
					}
					if after != before {
						return fmt.Errorf("count %d, then %d in one snapshot", before, after)
					}
					return nil
				})
				if err != nil && qctx.Err() == nil {
					t.Error(err)
				}
				cancel()
			}
		})
	}
	reads.Wait()
	close(stop)
	writes.Wait()
	d.Close()
	n.check(t)
}

// A live hub's workers hold read connections and prepare statements of
// their own on them. A Render that queries through QuerierFrom runs on
// those, one that queries its tx runs on the connection's cache, and
// every statement is closed once.
func TestLiveWorkersOnCachingConnections(t *testing.T) {
	ctx := context.Background()
	name, n := countingDriver(t)
	d := open(t, Options{Driver: name, Readers: 4})
	exec(t, d.W, "CREATE TABLE t (x INTEGER); INSERT INTO t VALUES (1), (2), (3)")
	const sum, all, upTo = "SELECT sum(x) FROM t", "SELECT x FROM t ORDER BY x", "SELECT count(*) FROM t WHERE x <= ?"
	var renders atomic.Int64
	view := live.View{Key: "v", Topics: []string{"t"}, Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
		q, ok := QuerierFrom(ctx)
		if !ok {
			return nil, errors.New("no Querier")
		}
		var a, b int
		if err := q.QueryRowContext(ctx, sum).Scan(&a); err != nil {
			return nil, err
		}
		if err := tx.QueryRowContext(ctx, sum).Scan(&b); err != nil {
			return nil, err
		}
		rows, err := q.QueryContext(ctx, all)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var x, c int
			if err := rows.Scan(&x); err != nil {
				return nil, err
			}
			if err := tx.QueryRowContext(ctx, upTo, x).Scan(&c); err != nil {
				return nil, err
			}
			if c != x {
				return nil, fmt.Errorf("%d rows up to %d", c, x)
			}
			if err := tx.QueryRowContext(ctx, all).Scan(&c); err != nil || c != 1 {
				return nil, fmt.Errorf("nested: %d, %v", c, err)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if a != 6 || b != 6 {
			return nil, fmt.Errorf("sums %d and %d", a, b)
		}
		renders.Add(1)
		return sse.PatchElements(fmt.Appendf(nil, `<p id="v">%d</p>`, a)), nil
	}}
	var logged syncBuffer
	h := live.New(live.Options{Read: d.R, Workers: 2, Tick: time.Millisecond, Log: slog.New(slog.NewTextHandler(&logged, nil))})
	hctx, stop := context.WithCancel(ctx)
	ran := make(chan error, 1)
	go func() { ran <- h.Run(hctx) }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Serve(w, r, live.Spec{Views: []live.View{view}})
	}))
	defer srv.Close()
	rctx, leave := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(rctx, "GET", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	go io.Copy(io.Discard, res.Body)
	deadline := time.Now().Add(5 * time.Second)
	for renders.Load() < 30 {
		if time.Now().After(deadline) {
			t.Fatalf("%d renders; the hub logged:\n%s", renders.Load(), logged.String())
		}
		h.Changed([]string{"t"})
		time.Sleep(time.Millisecond)
	}
	leave()
	stop()
	select {
	case err := <-ran:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hub did not stop")
	}
	res.Body.Close()
	if s := logged.String(); strings.Contains(s, "level=ERROR") {
		t.Fatalf("the hub logged:\n%s", s)
	}
	// Two workers, each with its own statement and its connection's.
	for _, q := range []string{sum, all, upTo} {
		if p, _ := n.of(q); p > 4 {
			t.Errorf("%q prepared %d times in %d renders", q, p, renders.Load())
		}
	}
	d.Close()
	n.check(t)
}
