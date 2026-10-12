package writer

import (
	"context"
	"database/sql"
	"errors"
)

// Tx is what a Command gets: the batch's transaction on the writer's
// connection. Its statements are prepared once, on that connection, and
// kept by their SQL text, so build queries from constants and pass values
// as arguments.
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
		return s.s.QueryRowContext(t.ctx, args...)
	}
	return t.w.conn.QueryRowContext(t.ctx, query, args...)
}

// Touch records that the command changed what the topics name. If the
// command succeeds, OnCommit gets them after the batch commits.
func (t *Tx) Touch(topics ...string) {
	t.live()
	t.topics = append(t.topics, topics...)
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
	err := t.w.call(Func(fn), t)
	if err != nil {
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
	t.rows = nil
}

// maxStmts bounds the statement cache. Queries built from constants never
// reach it; past it, statements are prepared for each call again.
const maxStmts = 512

type stmt struct {
	s    *sql.Stmt
	rows *sql.Rows // the last rows read through s, maybe still open
}

// prepared returns query's statement on the write connection, or nil if
// the query should run unprepared: when the cache is full, when the SQL
// has an error (the unprepared run reports it), or while rows of the
// statement are open.
func (w *Writer) prepared(query string) *stmt {
	s := w.stmts[query]
	if s == nil {
		if len(w.stmts) >= maxStmts {
			return nil
		}
		ps, err := w.conn.PrepareContext(context.Background(), query)
		if err != nil {
			return nil
		}
		s = &stmt{s: ps}
		w.stmts[query] = s
		return s
	}
	if s.rows != nil {
		if _, err := s.rows.Columns(); err == nil {
			return nil
		}
		s.rows = nil
	}
	return s
}

// exec runs a statement of the writer's own, outside any command.
func (w *Writer) exec(query string) error {
	var err error
	if s := w.prepared(query); s != nil {
		_, err = s.s.Exec()
	} else {
		_, err = w.conn.ExecContext(context.Background(), query)
	}
	return err
}

func (w *Writer) row(query string) *sql.Row {
	if s := w.prepared(query); s != nil {
		return s.s.QueryRow()
	}
	return w.conn.QueryRowContext(context.Background(), query)
}
