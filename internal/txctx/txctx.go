// Package txctx carries a transaction in a context: sqlite's WithTx and
// live's render workers put one in, sqlite's TxFrom and View take it out.
// It carries sqlite.Uncached's flag as well, which the statement caches of
// sqlite, live and writer read.
package txctx

import (
	"context"
	"database/sql"
	"sync"
)

// Querier is sqlite.Querier, which this package cannot import.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Value is a transaction in a context.
type Value struct {
	Tx *sql.Tx
	// Pool is the pool Tx came from, nil when the code that put it there
	// does not know.
	Pool *sql.DB
	// Querier runs queries in Tx with prepared statements; nil: Tx's own.
	Querier Querier

	mu    sync.Mutex
	file  string
	known bool
}

type key struct{}

// With returns a copy of ctx that carries v.
func With(ctx context.Context, v *Value) context.Context {
	return context.WithValue(ctx, key{}, v)
}

// From returns the value ctx carries, or nil.
func From(ctx context.Context) *Value {
	v, _ := ctx.Value(key{}).(*Value)
	return v
}

type uncachedKey struct{}

// WithUncached returns a copy of ctx whose queries skip the statement
// caches.
func WithUncached(ctx context.Context) context.Context {
	return context.WithValue(ctx, uncachedKey{}, true)
}

// Uncached reports whether ctx's queries skip the statement caches.
func Uncached(ctx context.Context) bool {
	u, _ := ctx.Value(uncachedKey{}).(bool)
	return u
}

// File returns the file of the database Tx reads, from lookup on the
// first call that succeeds.
func (v *Value) File(lookup func() (string, error)) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.known {
		f, err := lookup()
		if err != nil {
			return "", err
		}
		v.file, v.known = f, true
	}
	return v.file, nil
}
