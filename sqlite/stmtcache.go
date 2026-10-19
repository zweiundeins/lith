package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"reflect"

	"github.com/zweiundeins/lith/internal/txctx"
)

// stmtConnector opens the read pool's connections, each with a cache of
// the statements its queries ran, by their SQL text.
type stmtConnector struct {
	base driver.Connector
	max  int
}

// connector returns the connector sql.Open would use for the driver
// registered as name.
func connector(name, dsn string) (driver.Connector, error) {
	db, err := sql.Open(name, dsn)
	if err != nil {
		return nil, err
	}
	d := db.Driver()
	db.Close()
	if dc, ok := d.(driver.DriverContext); ok {
		return dc.OpenConnector(dsn)
	}
	return dsnConnector{dsn: dsn, d: d}, nil
}

type dsnConnector struct {
	dsn string
	d   driver.Driver
}

func (c dsnConnector) Connect(context.Context) (driver.Conn, error) { return c.d.Open(c.dsn) }
func (c dsnConnector) Driver() driver.Driver                        { return c.d }

// ctxConn is what the cache needs of a connection: database/sql's context
// methods, which modernc.org/sqlite's connections and dbmetrics' have.
type ctxConn interface {
	driver.Conn
	driver.ConnPrepareContext
	driver.ConnBeginTx
	driver.QueryerContext
	driver.ExecerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

func (c *stmtConnector) Connect(ctx context.Context) (driver.Conn, error) {
	dc, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	cc, ok := dc.(ctxConn)
	if !ok {
		return dc, nil // a driver without them goes without the cache
	}
	return &stmtConn{ctxConn: cc, max: c.max, stmts: make(map[string]*cachedStmt)}, nil
}

func (c *stmtConnector) Driver() driver.Driver { return c.base.Driver() }

// Close is called by sql.DB.Close.
func (c *stmtConnector) Close() error {
	if cl, ok := c.base.(io.Closer); ok {
		return cl.Close()
	}
	return nil
}

var (
	_ driver.QueryerContext                 = (*stmtConn)(nil)
	_ driver.ExecerContext                  = (*stmtConn)(nil)
	_ driver.ConnPrepareContext             = (*stmtConn)(nil)
	_ driver.ConnBeginTx                    = (*stmtConn)(nil)
	_ driver.SessionResetter                = (*stmtConn)(nil)
	_ driver.Validator                      = (*stmtConn)(nil)
	_ driver.NamedValueChecker              = (*stmtConn)(nil)
	_ driver.RowsColumnTypeScanType         = (*cachedRows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*cachedRows)(nil)
	_ driver.RowsColumnTypeLength           = (*cachedRows)(nil)
	_ driver.RowsColumnTypeNullable         = (*cachedRows)(nil)
	_ driver.RowsColumnTypePrecisionScale   = (*cachedRows)(nil)
	_ driver.RowsNextResultSet              = (*cachedRows)(nil)
)

// stmtConn is a read connection that keeps the statements its queries ran,
// the max used last. Everything but QueryContext goes to the driver's
// connection as it is. database/sql calls a connection, and the rows of
// its queries, under one lock, so the cache needs no lock of its own.
type stmtConn struct {
	ctxConn
	max   int
	stmts map[string]*cachedStmt
	lru   stmtList // the same statements, the one used last first
}

// cachedStmt is a statement prepared on the connection, kept by its SQL.
type cachedStmt struct {
	ds         queryStmt
	query      string
	rows       *cachedRows // its last query's, while open: it cannot run
	dropped    bool        // out of the cache while its rows were open
	prev, next *cachedStmt
}

type queryStmt interface {
	driver.Stmt
	driver.StmtQueryContext
}

// Unwrap returns the driver's connection, for code that reaches it through
// sql.Conn.Raw.
func (c *stmtConn) Unwrap() driver.Conn { return c.ctxConn }

func (c *stmtConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	var s *cachedStmt
	if !txctx.Uncached(ctx) {
		s = c.prepared(ctx, query)
	}
	if s == nil {
		return c.ctxConn.QueryContext(ctx, query, args)
	}
	rows, err := s.ds.QueryContext(ctx, args)
	if err != nil {
		return nil, err
	}
	s.rows = &cachedRows{Rows: rows, s: s}
	return s.rows, nil
}

// prepared returns query's statement, preparing it the first time, or nil
// when the query should run on a statement of its own, which its rows
// close: while the rows of the cached one are open, and when the SQL does
// not prepare (that run reports the error). A new statement in a full
// cache pushes out the one used longest ago.
func (c *stmtConn) prepared(ctx context.Context, query string) *cachedStmt {
	if s := c.stmts[query]; s != nil {
		c.lru.moveToFront(s)
		if s.rows != nil {
			return nil
		}
		return s
	}
	ds, err := c.PrepareContext(ctx, query)
	if err != nil {
		return nil
	}
	qs, ok := ds.(queryStmt)
	if !ok {
		ds.Close()
		return nil
	}
	if len(c.stmts) >= c.max {
		c.drop(c.lru.back)
	}
	s := &cachedStmt{ds: qs, query: query}
	c.stmts[query] = s
	c.lru.pushFront(s)
	return s
}

// drop takes s out of the cache and closes it, or leaves that to its rows
// while they are open.
func (c *stmtConn) drop(s *cachedStmt) {
	c.lru.remove(s)
	delete(c.stmts, s.query)
	if s.rows != nil {
		s.dropped = true
		return
	}
	s.ds.Close()
}

// Close closes the cached statements, then the driver's connection.
func (c *stmtConn) Close() error {
	for c.lru.back != nil {
		c.drop(c.lru.back)
	}
	return c.ctxConn.Close()
}

func (c *stmtConn) CheckNamedValue(nv *driver.NamedValue) error {
	if n, ok := c.ctxConn.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

// cachedRows are the rows of a cached statement: closing them frees it.
type cachedRows struct {
	driver.Rows
	s *cachedStmt
}

func (r *cachedRows) Close() error {
	err := r.Rows.Close()
	if s := r.s; s.rows == r {
		s.rows = nil
		if s.dropped {
			s.ds.Close()
		}
	}
	return err
}

func (r *cachedRows) ColumnTypeScanType(i int) reflect.Type {
	if c, ok := r.Rows.(driver.RowsColumnTypeScanType); ok {
		return c.ColumnTypeScanType(i)
	}
	return reflect.TypeFor[any]()
}

func (r *cachedRows) ColumnTypeDatabaseTypeName(i int) string {
	if c, ok := r.Rows.(driver.RowsColumnTypeDatabaseTypeName); ok {
		return c.ColumnTypeDatabaseTypeName(i)
	}
	return ""
}

func (r *cachedRows) ColumnTypeLength(i int) (int64, bool) {
	if c, ok := r.Rows.(driver.RowsColumnTypeLength); ok {
		return c.ColumnTypeLength(i)
	}
	return 0, false
}

func (r *cachedRows) ColumnTypeNullable(i int) (nullable, ok bool) {
	if c, isImpl := r.Rows.(driver.RowsColumnTypeNullable); isImpl {
		return c.ColumnTypeNullable(i)
	}
	return false, false
}

func (r *cachedRows) ColumnTypePrecisionScale(i int) (precision, scale int64, ok bool) {
	if c, isImpl := r.Rows.(driver.RowsColumnTypePrecisionScale); isImpl {
		return c.ColumnTypePrecisionScale(i)
	}
	return 0, 0, false
}

func (r *cachedRows) HasNextResultSet() bool {
	if n, ok := r.Rows.(driver.RowsNextResultSet); ok {
		return n.HasNextResultSet()
	}
	return false
}

func (r *cachedRows) NextResultSet() error {
	if n, ok := r.Rows.(driver.RowsNextResultSet); ok {
		return n.NextResultSet()
	}
	return io.EOF
}

// stmtList is a doubly linked list of statements.
type stmtList struct {
	front, back *cachedStmt
}

func (l *stmtList) pushFront(s *cachedStmt) {
	s.prev, s.next = nil, l.front
	if l.front != nil {
		l.front.prev = s
	} else {
		l.back = s
	}
	l.front = s
}

func (l *stmtList) remove(s *cachedStmt) {
	if s.prev != nil {
		s.prev.next = s.next
	} else {
		l.front = s.next
	}
	if s.next != nil {
		s.next.prev = s.prev
	} else {
		l.back = s.prev
	}
	s.prev, s.next = nil, nil
}

func (l *stmtList) moveToFront(s *cachedStmt) {
	if l.front != s {
		l.remove(s)
		l.pushFront(s)
	}
}
