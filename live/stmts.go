package live

import (
	"context"
	"database/sql"
	"sync"
)

// maxStmts bounds a worker's statement cache. Queries built from
// constants never reach it; past it, queries run unprepared.
const maxStmts = 512

// stmt is a statement a worker prepared on its connection.
type stmt struct {
	s      *sql.Stmt
	rows   *sql.Rows // of its last Query, maybe still open
	row    *sql.Row  // of its last QueryRow, maybe not scanned yet
	listed bool      // in reads.used
}

// free reports whether s can run again, scanning the row of its last
// QueryRow if that is still pending: only rows of a Query may still be
// read.
func (s *stmt) free() bool {
	if s.row != nil {
		s.row.Scan() // with nothing to scan into, it only closes the row
		s.row = nil
	}
	if s.rows != nil {
		if _, err := s.rows.Columns(); err == nil {
			return false
		}
		s.rows = nil
	}
	return true
}

// reads is the Querier a worker's Render finds in its ctx: the tick's
// transaction, with the worker's prepared statements.
type reads struct {
	w  *worker
	tx *sql.Tx

	mu   sync.Mutex
	used []*stmt // by the Render under way
	done bool    // the tick is over
}

func (q *reads) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return q.tx.ExecContext(ctx, query, args...)
}

func (q *reads) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if s := q.prepared(ctx, query); s != nil {
		rows, err := s.s.QueryContext(ctx, args...)
		if err == nil {
			s.rows = rows
		}
		return rows, err
	}
	return q.tx.QueryContext(ctx, query, args...)
}

func (q *reads) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	q.mu.Lock()
	defer q.mu.Unlock()
	if s := q.prepared(ctx, query); s != nil {
		s.row = s.s.QueryRowContext(ctx, args...)
		return s.row
	}
	return q.tx.QueryRowContext(ctx, query, args...)
}

// prepared returns query's statement if it can run now, preparing it the
// first time; nil when the query should run on the transaction instead:
// after the tick, while rows of the statement are open, when the cache
// is full, or when the SQL does not prepare (the transaction reports its
// error). q.mu is held.
func (q *reads) prepared(ctx context.Context, query string) *stmt {
	if q.done {
		return nil
	}
	s := q.w.stmts[query]
	if s == nil {
		if len(q.w.stmts) >= maxStmts {
			return nil
		}
		ps, err := q.w.conn.PrepareContext(ctx, query)
		if err != nil {
			return nil
		}
		s = &stmt{s: ps}
		q.w.stmts[query] = s
	} else if !s.free() {
		return nil
	}
	if !s.listed {
		s.listed = true
		q.used = append(q.used, s)
	}
	return s
}

// release closes what the Render left open, so that the tick's
// transaction can end; after the tick, queries go to the ended
// transaction and fail.
func (q *reads) release(endTick bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, s := range q.used {
		s.free()
		if s.rows != nil {
			s.rows.Close()
			s.rows = nil
		}
		s.listed = false
	}
	clear(q.used)
	q.used = q.used[:0]
	q.done = endTick
}

// closeStmts closes the worker's statements, before its connection goes.
func (w *worker) closeStmts() {
	for _, s := range w.stmts {
		s.s.Close()
	}
	clear(w.stmts)
}
