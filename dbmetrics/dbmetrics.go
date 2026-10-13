// Package dbmetrics times the statements a database/sql driver runs and logs
// the slow ones.
//
// Wrap a connector (sql.OpenDB) or a driver (sql.Register): each query and
// exec goes to an Observer and, at or above the slow threshold, to the log
// with its SQL text, never its arguments. A query's time is the time spent in
// the driver, in the call and in every Next, not the caller's work between
// rows. The Prometheus Observer is the nested module dbmetrics/prom, so this
// package pulls in no metrics library.
package dbmetrics

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"time"
)

// The op an Observer receives: a statement that returns rows, or one that
// does not.
const (
	OpQuery = "query"
	OpExec  = "exec"
)

// DefaultSlowThreshold is the slow threshold when Options leaves it zero.
const DefaultSlowThreshold = 100 * time.Millisecond

// Observer receives the duration of every statement. slow reports whether it
// reached the slow threshold. Observe must be safe for concurrent use.
type Observer interface {
	Observe(op string, elapsed time.Duration, slow bool)
}

// Options configure a wrapped driver or connector.
type Options struct {
	// Observer receives every statement; nil records nothing but the slow log.
	Observer Observer
	// SlowThreshold is the duration from which a statement is slow: logged at
	// Warn and flagged to the Observer. Zero means DefaultSlowThreshold, a
	// negative value turns the slow log off.
	SlowThreshold time.Duration
	// Logger receives the slow log; nil means slog.Default().
	Logger *slog.Logger
}

type recorder struct {
	obs  Observer
	slow time.Duration
	log  *slog.Logger
}

func newRecorder(o Options) *recorder {
	r := &recorder{obs: o.Observer, slow: o.SlowThreshold, log: o.Logger}
	if r.slow == 0 {
		r.slow = DefaultSlowThreshold
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	return r
}

func (r *recorder) record(op, query string, elapsed time.Duration) {
	slow := r.slow > 0 && elapsed >= r.slow
	if r.obs != nil {
		r.obs.Observe(op, elapsed, slow)
	}
	if slow {
		r.log.Warn("slow db query", "op", op, "duration_ms", elapsed.Milliseconds(), "query", oneLine(query))
	}
}

// oneLine collapses whitespace and truncates, so a slow query logs as one row.
func oneLine(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	const limit = 300
	if len(q) > limit {
		return q[:limit] + "…"
	}
	return q
}

// WrapConnector returns a connector whose connections time their statements:
//
//	c, err := sqlite.NewConnector(dsn)
//	...
//	db := sql.OpenDB(dbmetrics.WrapConnector(c, opts))
func WrapConnector(c driver.Connector, opts Options) driver.Connector {
	return &connector{base: c, rec: newRecorder(opts)}
}

// Wrap returns a driver whose connections time their statements.
func Wrap(d driver.Driver, opts Options) driver.Driver {
	return &wrappedDriver{base: d, rec: newRecorder(opts)}
}

// Register registers under name the driver registered as base, wrapped. It
// fails if name is taken or base is unknown. sqlite.Options.Driver opens a
// lith database through it.
func Register(name, base string, opts Options) error {
	if slices.Contains(sql.Drivers(), name) {
		return fmt.Errorf("dbmetrics: driver %q is already registered", name)
	}
	db, err := sql.Open(base, "")
	if err != nil {
		return fmt.Errorf("dbmetrics: %w", err)
	}
	d := db.Driver()
	_ = db.Close()
	sql.Register(name, Wrap(d, opts))
	return nil
}

type wrappedDriver struct {
	base driver.Driver
	rec  *recorder
}

func (d *wrappedDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &conn{Conn: c, rec: d.rec}, nil
}

type connector struct {
	base driver.Connector
	rec  *recorder
}

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	dc, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &conn{Conn: dc, rec: c.rec}, nil
}

func (c *connector) Driver() driver.Driver { return &wrappedDriver{base: c.base.Driver(), rec: c.rec} }

// Close is called by sql.DB.Close.
func (c *connector) Close() error {
	if cl, ok := c.base.(io.Closer); ok {
		return cl.Close()
	}
	return nil
}

// conn times the driver's statements and forwards the optional interfaces
// of a context-aware driver, falling back as database/sql does when the
// driver lacks one.
type conn struct {
	driver.Conn
	rec *recorder
}

var (
	_ driver.QueryerContext     = (*conn)(nil)
	_ driver.ExecerContext      = (*conn)(nil)
	_ driver.ConnPrepareContext = (*conn)(nil)
	_ driver.ConnBeginTx        = (*conn)(nil)
	_ driver.Pinger             = (*conn)(nil)
	_ driver.Validator          = (*conn)(nil)
	_ driver.SessionResetter    = (*conn)(nil)
	_ driver.NamedValueChecker  = (*conn)(nil)
)

// Unwrap returns the driver's connection. Code that reaches it through
// sql.Conn.Raw for driver-specific methods (commit hooks, FileControl) must
// unwrap first.
func (c *conn) Unwrap() driver.Conn { return c.Conn }

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	start := time.Now()
	var rows driver.Rows
	var err error
	switch q := c.Conn.(type) {
	case driver.QueryerContext:
		rows, err = q.QueryContext(ctx, query, args)
	case driver.Queryer: //nolint:staticcheck // the fallback database/sql uses too
		var vals []driver.Value
		if vals, err = legacyArgs(ctx, args); err == nil {
			rows, err = q.Query(query, vals)
		}
	default:
		return nil, driver.ErrSkip
	}
	return c.rec.rows(query, rows, err, time.Since(start))
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	start := time.Now()
	var res driver.Result
	var err error
	switch e := c.Conn.(type) {
	case driver.ExecerContext:
		res, err = e.ExecContext(ctx, query, args)
	case driver.Execer: //nolint:staticcheck // the fallback database/sql uses too
		var vals []driver.Value
		if vals, err = legacyArgs(ctx, args); err == nil {
			res, err = e.Exec(query, vals)
		}
	default:
		return nil, driver.ErrSkip
	}
	if err != driver.ErrSkip {
		c.rec.record(OpExec, query, time.Since(start))
	}
	return res, err
}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	s, err := c.Conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	return c.stmt(s, query), nil
}

// stmt wraps a driver statement, keeping its ColumnConverter visible to
// database/sql when it has one.
func (c *conn) stmt(s driver.Stmt, query string) driver.Stmt {
	w := &stmt{Stmt: s, conn: c.Conn, query: query, rec: c.rec}
	if _, ok := s.(driver.ColumnConverter); ok { //nolint:staticcheck // still consulted by database/sql
		return converterStmt{w}
	}
	return w
}

func (c *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	p, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		s, err := c.Prepare(query)
		if err == nil && ctx.Err() != nil {
			_ = s.Close()
			return nil, ctx.Err()
		}
		return s, err
	}
	s, err := p.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return c.stmt(s, query), nil
}

func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	if opts.Isolation != driver.IsolationLevel(sql.LevelDefault) {
		return nil, errors.New("sql: driver does not support non-default isolation level")
	}
	if opts.ReadOnly {
		return nil, errors.New("sql: driver does not support read-only transactions")
	}
	tx, err := c.Conn.Begin() //nolint:staticcheck // the fallback database/sql uses too
	if err == nil && ctx.Err() != nil {
		_ = tx.Rollback()
		return nil, ctx.Err()
	}
	return tx, err
}

func (c *conn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *conn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *conn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *conn) CheckNamedValue(nv *driver.NamedValue) error {
	if n, ok := c.Conn.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

type stmt struct {
	driver.Stmt
	conn  driver.Conn // the driver's
	query string
	rec   *recorder
}

var (
	_ driver.StmtQueryContext  = (*stmt)(nil)
	_ driver.StmtExecContext   = (*stmt)(nil)
	_ driver.NamedValueChecker = (*stmt)(nil)
)

func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	start := time.Now()
	var rows driver.Rows
	var err error
	if q, ok := s.Stmt.(driver.StmtQueryContext); ok {
		rows, err = q.QueryContext(ctx, args)
	} else {
		var vals []driver.Value
		if vals, err = legacyArgs(ctx, args); err == nil {
			rows, err = s.Stmt.Query(vals) //nolint:staticcheck // the fallback database/sql uses too
		}
	}
	return s.rec.rows(s.query, rows, err, time.Since(start))
}

func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	start := time.Now()
	var res driver.Result
	var err error
	if e, ok := s.Stmt.(driver.StmtExecContext); ok {
		res, err = e.ExecContext(ctx, args)
	} else {
		var vals []driver.Value
		if vals, err = legacyArgs(ctx, args); err == nil {
			res, err = s.Stmt.Exec(vals) //nolint:staticcheck // the fallback database/sql uses too
		}
	}
	s.rec.record(OpExec, s.query, time.Since(start))
	return res, err
}

// legacyArgs converts arguments for a statement without the context methods,
// as database/sql would.
func legacyArgs(ctx context.Context, args []driver.NamedValue) ([]driver.Value, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	vals := make([]driver.Value, len(args))
	for i, a := range args {
		if a.Name != "" {
			return nil, errors.New("sql: driver does not support the use of Named Parameters")
		}
		vals[i] = a.Value
	}
	return vals, nil
}

// CheckNamedValue asks the driver's statement, then its connection, as
// database/sql would.
func (s *stmt) CheckNamedValue(nv *driver.NamedValue) error {
	if n, ok := s.Stmt.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	if n, ok := s.conn.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

type converterStmt struct{ *stmt }

func (s converterStmt) ColumnConverter(idx int) driver.ValueConverter {
	return s.Stmt.(driver.ColumnConverter).ColumnConverter(idx) //nolint:staticcheck // still consulted by database/sql
}

// rows records a failed query at once and a successful one when its rows are
// closed, with the time spent in Next added.
func (r *recorder) rows(query string, dr driver.Rows, err error, elapsed time.Duration) (driver.Rows, error) {
	if err != nil {
		if err != driver.ErrSkip {
			r.record(OpQuery, query, elapsed)
		}
		return nil, err
	}
	return &rows{Rows: dr, query: query, rec: r, elapsed: elapsed}, nil
}

type rows struct {
	driver.Rows
	query    string
	rec      *recorder
	elapsed  time.Duration
	recorded bool
}

var (
	_ driver.RowsColumnTypeScanType         = (*rows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*rows)(nil)
	_ driver.RowsColumnTypeLength           = (*rows)(nil)
	_ driver.RowsColumnTypeNullable         = (*rows)(nil)
	_ driver.RowsColumnTypePrecisionScale   = (*rows)(nil)
	_ driver.RowsNextResultSet              = (*rows)(nil)
)

func (r *rows) Next(dest []driver.Value) error {
	start := time.Now()
	err := r.Rows.Next(dest)
	r.elapsed += time.Since(start)
	return err
}

func (r *rows) Close() error {
	err := r.Rows.Close()
	if !r.recorded {
		r.recorded = true
		r.rec.record(OpQuery, r.query, r.elapsed)
	}
	return err
}

func (r *rows) ColumnTypeScanType(i int) reflect.Type {
	if c, ok := r.Rows.(driver.RowsColumnTypeScanType); ok {
		return c.ColumnTypeScanType(i)
	}
	return reflect.TypeFor[any]()
}

func (r *rows) ColumnTypeDatabaseTypeName(i int) string {
	if c, ok := r.Rows.(driver.RowsColumnTypeDatabaseTypeName); ok {
		return c.ColumnTypeDatabaseTypeName(i)
	}
	return ""
}

func (r *rows) ColumnTypeLength(i int) (int64, bool) {
	if c, ok := r.Rows.(driver.RowsColumnTypeLength); ok {
		return c.ColumnTypeLength(i)
	}
	return 0, false
}

func (r *rows) ColumnTypeNullable(i int) (nullable, ok bool) {
	if c, isImpl := r.Rows.(driver.RowsColumnTypeNullable); isImpl {
		return c.ColumnTypeNullable(i)
	}
	return false, false
}

func (r *rows) ColumnTypePrecisionScale(i int) (precision, scale int64, ok bool) {
	if c, isImpl := r.Rows.(driver.RowsColumnTypePrecisionScale); isImpl {
		return c.ColumnTypePrecisionScale(i)
	}
	return 0, 0, false
}

func (r *rows) HasNextResultSet() bool {
	if n, ok := r.Rows.(driver.RowsNextResultSet); ok {
		return n.HasNextResultSet()
	}
	return false
}

func (r *rows) NextResultSet() error {
	if n, ok := r.Rows.(driver.RowsNextResultSet); ok {
		return n.NextResultSet()
	}
	return io.EOF
}
