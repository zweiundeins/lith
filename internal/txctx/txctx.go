// Package txctx carries a transaction in a context: sqlite's WithTx and
// live's render workers put one in, sqlite's TxFrom and View take it out.
package txctx

import (
	"context"
	"database/sql"
	"sync"
)

// Value is a transaction in a context.
type Value struct {
	Tx *sql.Tx
	// Pool is the pool Tx came from, nil when the code that put it there
	// does not know.
	Pool *sql.DB

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
