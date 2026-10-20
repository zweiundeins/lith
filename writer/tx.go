package writer

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zweiundeins/lith/internal/txctx"
	"github.com/zweiundeins/lith/sqlite"
)

// Tx is what a Command gets: the batch's transaction on the writer's
// connection. Its statements are prepared once, on that connection, and
// kept by their SQL text, the 512 used last. Build queries from constants
// and pass values as arguments; SQL built at runtime, such as an IN list
// with a mark per value, runs through ExecUncached, QueryUncached and
// QueryRowUncached, which keep no statement, as do ExecContext,
// QueryContext and QueryRowContext with a ctx from sqlite.Uncached.
type Tx struct {
	w      *Writer
	ctx    context.Context
	topics []string
	after  []func()
	rows   []*sql.Rows // closed when the command returns
	done   bool
}

// Context returns the context of the Do that queued the command, never
// cancelled, or context.Background() for Send.
func (t *Tx) Context() context.Context { return t.ctx }

// Exec runs a statement.
func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	t.live()
	if s := t.w.prepared(query); s != nil {
		s.row = false
		return s.s.ExecContext(t.ctx, args...)
	}
	return t.w.conn.ExecContext(t.ctx, query, args...)
}

// Query runs a query. While its rows are open, the same query runs on a
// statement of its own. Rows still open when the command returns are
// closed then.
func (t *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	t.live()
	var rows *sql.Rows
	var err error
	if s := t.w.prepared(query); s != nil {
		s.row = false
		if rows, err = s.s.QueryContext(t.ctx, args...); err == nil {
			s.rows = rows
		}
	} else {
		rows, err = t.w.conn.QueryContext(t.ctx, query, args...)
	}
	if err == nil {
		t.rows = append(t.rows, rows)
	}
	return rows, err
}

// QueryRow runs a query for one row. Scan the row before running the same
// query again: until then its statement is busy.
func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	t.live()
	if s := t.w.prepared(query); s != nil {
		s.row = true
		return s.s.QueryRowContext(t.ctx, args...)
	}
	return t.w.conn.QueryRowContext(t.ctx, query, args...)
}

// ExecUncached is Exec for SQL built at runtime, such as an IN list with
// a mark per value: the statement is prepared for this call only, instead
// of taking a place in the cache for each length of the list.
func (t *Tx) ExecUncached(query string, args ...any) (sql.Result, error) {
	t.live()
	return t.w.conn.ExecContext(t.ctx, query, args...)
}

// QueryUncached is Query for SQL built at runtime, without the statement
// cache. Rows still open when the command returns are closed then.
func (t *Tx) QueryUncached(query string, args ...any) (*sql.Rows, error) {
	t.live()
	rows, err := t.w.conn.QueryContext(t.ctx, query, args...)
	if err == nil {
		t.rows = append(t.rows, rows)
	}
	return rows, err
}

// QueryRowUncached is QueryRow for SQL built at runtime, without the
// statement cache.
func (t *Tx) QueryRowUncached(query string, args ...any) *sql.Row {
	t.live()
	return t.w.conn.QueryRowContext(t.ctx, query, args...)
}

// ExecContext is Exec, for code that takes a sqlite.Querier. Like the
// other methods it runs on the batch's transaction, with its statement
// cache and the command's context: ctx is not used, so no statement is
// cancelled halfway through a batch, except that with a ctx from
// sqlite.Uncached it is ExecUncached.
func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if txctx.Uncached(ctx) {
		return t.ExecUncached(query, args...)
	}
	return t.Exec(query, args...)
}

// QueryContext is Query, for code that takes a sqlite.Querier; ctx is not
// used, except that with a ctx from sqlite.Uncached it is QueryUncached.
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if txctx.Uncached(ctx) {
		return t.QueryUncached(query, args...)
	}
	return t.Query(query, args...)
}

// QueryRowContext is QueryRow, for code that takes a sqlite.Querier; ctx
// is not used, except that with a ctx from sqlite.Uncached it is
// QueryRowUncached.
func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if txctx.Uncached(ctx) {
		return t.QueryRowUncached(query, args...)
	}
	return t.QueryRow(query, args...)
}

var _ sqlite.Querier = (*Tx)(nil)

// Touch records that the command changed what the topics name. If the
// command succeeds, OnCommit gets them after the batch commits.
func (t *Tx) Touch(topics ...string) {
	t.live()
	t.topics = append(t.topics, topics...)
}

// Untouched runs fn and drops the topics it touched, while its changes
// and its AfterCommit functions stay: for writes that no view reads yet,
// such as a table being filled to replace another. It returns fn's error
// and undoes nothing; run fn in a Savepoint for that.
func (t *Tx) Untouched(fn func(*Tx) error) error {
	t.live()
	n := len(t.topics)
	err := fn(t)
	t.topics = t.topics[:n]
	return err
}

// AfterCommit runs fn on the writer's goroutine once the batch has
// committed, before Do returns, in the order the commands registered
// their functions. If the command fails, or the savepoint fn was
// registered in, fn is dropped. fn must not block.
func (t *Tx) AfterCommit(fn func()) {
	t.live()
	t.after = append(t.after, fn)
}

// Savepoint runs fn in a nested savepoint. If fn returns an error or
// panics, its changes are rolled back, the topics it touched and the
// functions it registered are dropped, and its error is returned: the
// command can go on.
func (t *Tx) Savepoint(fn func(*Tx) error) error {
	t.live()
	if err := t.w.exec("SAVEPOINT nested"); err != nil {
		return err
	}
	topics, after := len(t.topics), len(t.after)
	err, keep := unkept(t.w.call(Func(fn), t))
	if !keep {
		t.topics, t.after = t.topics[:topics], t.after[:after]
		if e := t.w.exec("ROLLBACK TO nested"); e != nil {
			return errors.Join(err, e)
		}
	}
	if e := t.w.exec("RELEASE nested"); e != nil {
		return errors.Join(err, e)
	}
	return err
}

// Local returns the value SetLocal stored under key on this writer, or
// nil. Packages built on the writer keep there what they know of its
// database, such as whether their tables exist: one package-level value
// can then serve the databases of many writers, a database per account
// among them. Commands run one at a time, so Local needs no lock.
func (t *Tx) Local(key any) any {
	t.live()
	return t.w.locals[key]
}

// SetLocal stores value under key on this writer, for the commands that
// follow, until the writer stops. It is not undone when the command
// fails: set what depends on the commit in AfterCommit.
func (t *Tx) SetLocal(key, value any) {
	t.live()
	if t.w.locals == nil {
		t.w.locals = map[any]any{}
	}
	t.w.locals[key] = value
}

func (t *Tx) live() {
	if t.done {
		panic("writer: Tx used after its command returned")
	}
}

func (t *Tx) end() {
	t.done = true
	for _, r := range t.rows {
		r.Close()
	}
	clear(t.rows)
	t.w.closeRetired()
}

// maxStmts bounds the statement cache, which keeps the statements used
// last: SQL built at runtime pushes out the ones used longest ago, and
// those are prepared again when they come back.
const maxStmts = 512

type stmt struct {
	s     *sql.Stmt
	query string
	rows  *sql.Rows // the last rows read through s, maybe still open
	row   bool      // its last run was a QueryRow, whose row may not be scanned yet
}

// reading reports whether the rows of s's last Query are still open.
func (s *stmt) reading() bool {
	if s.rows == nil {
		return false
	}
	if _, err := s.rows.Columns(); err == nil {
		return true
	}
	s.rows = nil
	return false
}

// prepared returns query's statement on the write connection, or nil if
// the query should run unprepared: when the SQL has an error (the
// unprepared run reports it), or while rows of the statement are open. A
// new statement in a full cache pushes out the one used longest ago.
func (w *Writer) prepared(query string) *stmt {
	if e := w.stmts[query]; e != nil {
		w.lru.MoveToFront(e)
		s := e.Value.(*stmt)
		if s.reading() {
			return nil
		}
		return s
	}
	ps, err := w.conn.PrepareContext(context.Background(), query)
	if err != nil {
		return nil
	}
	if w.lru.Len() >= maxStmts {
		old := w.lru.Remove(w.lru.Back()).(*stmt)
		delete(w.stmts, old.query)
		if old.row || old.reading() {
			// Closing it would pull the rows from under the command.
			w.retired = append(w.retired, old.s)
		} else {
			old.s.Close()
		}
	}
	s := &stmt{s: ps, query: query}
	w.stmts[query] = w.lru.PushFront(s)
	return s
}

// closeRetired closes the statements pushed out of the cache while a
// command could still read their rows. Call it once that command ended.
func (w *Writer) closeRetired() {
	for _, s := range w.retired {
		s.Close()
	}
	clear(w.retired)
	w.retired = w.retired[:0]
}

// exec runs a statement of the writer's own, outside any command.
func (w *Writer) exec(query string) error {
	var err error
	if s := w.prepared(query); s != nil {
		s.row = false
		_, err = s.s.Exec()
	} else {
		_, err = w.conn.ExecContext(context.Background(), query)
	}
	return err
}

func (w *Writer) row(query string) *sql.Row {
	if s := w.prepared(query); s != nil {
		s.row = true
		return s.s.QueryRow()
	}
	return w.conn.QueryRowContext(context.Background(), query)
}
