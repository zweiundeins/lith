package live

import (
	"context"
	"database/sql"
	"sync"

	"github.com/zweiundeins/lith/internal/txctx"
)

// maxStmts bounds a worker's statement cache, which keeps the statements
// used last: SQL built at runtime pushes out the ones used longest ago,
// and those are prepared again when they come back.
const maxStmts = 512

// stmt is a statement a worker prepared on its connection.
type stmt struct {
	s          *sql.Stmt
	query      string
	rows       *sql.Rows // of its last Query, maybe still open
	row        *sql.Row  // of its last QueryRow, maybe not scanned yet
	listed     bool      // in reads.used
	prev, next *stmt     // in worker.lru
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

// free reports whether s can run again, scanning the row of its last
// QueryRow if that is still pending: only rows of a Query may still be
// read.
func (s *stmt) free() bool {
	if s.row != nil {
		s.row.Scan() // with nothing to scan into, it only closes the row
		s.row = nil
	}
	return !s.reading()
}

// stmtList is a doubly linked list of statements.
type stmtList struct {
	front, back *stmt
}

func (l *stmtList) pushFront(s *stmt) {
	s.prev, s.next = nil, l.front
	if l.front != nil {
		l.front.prev = s
	} else {
		l.back = s
	}
	l.front = s
}

func (l *stmtList) remove(s *stmt) {
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

func (l *stmtList) moveToFront(s *stmt) {
	if l.front != s {
		l.remove(s)
		l.pushFront(s)
	}
}

// reads is the Querier a worker's Render finds in its ctx: the tick's
// transaction, with the worker's prepared statements.
type reads struct {
	w  *worker
	tx *sql.Tx

	mu      sync.Mutex
	used    []*stmt     // by the Render under way
	retired []*sql.Stmt // pushed out of the cache while that Render could still read them
	done    bool        // the tick is over
}

func (q *reads) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return q.tx.ExecContext(ctx, query, args...)
}

func (q *reads) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if txctx.Uncached(ctx) {
		return q.tx.QueryContext(ctx, query, args...)
	}
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
	if txctx.Uncached(ctx) {
		return q.tx.QueryRowContext(ctx, query, args...)
	}
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
// after the tick, while rows of the statement are open, or when the SQL
// does not prepare (the transaction reports its error). A new statement
// in a full cache pushes out the one used longest ago. q.mu is held.
func (q *reads) prepared(ctx context.Context, query string) *stmt {
	if q.done {
		return nil
	}
	w := q.w
	s := w.stmts[query]
	if s != nil {
		w.lru.moveToFront(s)
		if !s.free() {
			return nil
		}
	} else {
		ps, err := w.conn.PrepareContext(ctx, query)
		if err != nil {
			return nil
		}
		if len(w.stmts) >= maxStmts {
			old := w.lru.back
			w.lru.remove(old)
			delete(w.stmts, old.query)
			if old.row != nil || old.reading() {
				// Closing it would pull the rows from under the Render.
				q.retired = append(q.retired, old.s)
			} else {
				old.s.Close()
			}
		}
		s = &stmt{s: ps, query: query}
		w.stmts[query] = s
		w.lru.pushFront(s)
	}
	if !s.listed {
		s.listed = true
		q.used = append(q.used, s)
	}
	return s
}

// release closes what the Render left open, so that the tick's
// transaction can end, and then the statements pushed out of the cache
// during the Render; after the tick, queries go to the ended transaction
// and fail.
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
	for _, s := range q.retired {
		s.Close()
	}
	clear(q.retired)
	q.retired = q.retired[:0]
	q.done = endTick
}

// closeStmts closes the worker's statements, before its connection goes.
func (w *worker) closeStmts() {
	for _, s := range w.stmts {
		s.s.Close()
	}
	clear(w.stmts)
	w.lru = stmtList{}
}
