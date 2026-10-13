---
title: tenant
summary: Keeps one open value per key, usually a SQLite database per account.
order: 6
---

Import path `github.com/zweiundeins/lith/tenant`. Generated from the doc comments in [tenant/](https://github.com/zweiundeins/lith/tree/main/tenant).

A Pool opens a value on first use and keeps it while anyone holds a pin on it. Idle values are closed, least recently released first, when a cold open would put more than maxOpen values open. The cap is soft: Get never waits for a free slot, so pinned values can exceed it.

Opens and closes run outside the pool's lock. A key that is being opened or closed is busy, and everyone else who needs that key waits for it, so a slow open (a restore from a replica) holds up only its own key and a key is never open twice.

## Variables

```go
var (
	// ErrClosed is returned by Get after Close.
	ErrClosed = errors.New("tenant: pool closed")
	// ErrDenied is the reason Get gives for a key denied without one.
	ErrDenied = errors.New("tenant: key denied")
)
```

## Types

### type Opener {#Opener}

```go
type Opener[T io.Closer] func(ctx context.Context, key string) (T, error)
```

Opener opens the value for key. Its context carries the values of the Get that needed it but not its cancellation: a request that gives up must not abort a restore halfway.

### type Option {#Option}

```go
type Option func(*options)
```

Option configures a Pool.

#### func WithLogger {#WithLogger}

```go
func WithLogger(l *slog.Logger) Option
```

WithLogger sets where failed closes of evicted values are logged; the default is slog.Default().

### type Pool {#Pool}

```go
type Pool[T io.Closer] struct {
	// contains filtered or unexported fields
}
```

Pool is a set of open values keyed by string. It is safe for concurrent use.

#### func New {#New}

```go
func New[T io.Closer](maxOpen int, open Opener[T], opts ...Option) *Pool[T]
```

New returns a pool that opens values with open and closes idle ones beyond maxOpen, which must be at least 1.

#### func (*Pool[T]) Allow {#Pool.Allow}

```go
func (p *Pool[T]) Allow(key string)
```

Allow lifts a Deny.

#### func (*Pool[T]) Close {#Pool.Close}

```go
func (p *Pool[T]) Close() error
```

Close waits for opens and closes in flight, then closes every value, pinned or not, and makes later Gets fail with ErrClosed. It returns the values' close errors.

#### func (*Pool[T]) Deny {#Pool.Deny}

```go
func (p *Pool[T]) Deny(key string, reason error)
```

Deny makes Get fail with reason (ErrDenied if nil) for key until Allow. An open in flight is discarded when it finishes; a value already open stays open until Evict. Deny, then Evict, leaves the key closed for good, as an erasure or a restore needs.

#### func (*Pool[T]) Do {#Pool.Do}

```go
func (p *Pool[T]) Do(ctx context.Context, key string, fn func(T) error) error
```

Do runs fn with the value for key pinned for the length of the call.

#### func (*Pool[T]) Evict {#Pool.Evict}

```go
func (p *Pool[T]) Evict(ctx context.Context, key string) error
```

Evict closes the value for key now, pinned or not, after any open or close of key in flight; ctx bounds only that wait. Pins held on the closed value go stale: releasing them does nothing. Evicting a key that is not open is a no-op.

#### func (*Pool[T]) Get {#Pool.Get}

```go
func (p *Pool[T]) Get(ctx context.Context, key string) (val T, release func(), err error)
```

Get returns the value for key, opening it on first use, and pins it until release is called. release may be called more than once.

Hold a pin for one unit of work, a request or a render. An SSE stream that pins its tenant for its whole life keeps it from ever being evicted and pushes the pool past maxOpen: release between renders and re-acquire, or use Do for each render.

ctx bounds the wait for another open or close of key; a cold open started by this call runs to completion. Gets that waited on an open that failed return its error rather than each trying again.

#### func (*Pool[T]) Stats {#Pool.Stats}

```go
func (p *Pool[T]) Stats() Stats
```

Stats reports the pool's occupancy.

### type Stats {#Stats}

```go
type Stats struct {
	Open      int    // values open
	Pinned    int    // open values with at least one pin
	Opening   int    // opens in flight
	MaxOpen   int    // the soft cap
	Opens     uint64 // successful opens since New
	Evictions uint64 // idle values closed to stay within MaxOpen
}
```

Stats is a snapshot of a pool.

Pinned close to MaxOpen is the condition to alert on: nothing is left to evict, and every cold open grows the pool past its cap.
