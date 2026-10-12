// Package tenant keeps one open value per key, usually a SQLite database
// per account.
//
// A Pool opens a value on first use and keeps it while anyone holds a pin on
// it. Idle values are closed, least recently released first, when a cold
// open would put more than maxOpen values open. The cap is soft: Get never
// waits for a free slot, so pinned values can exceed it.
//
// Opens and closes run outside the pool's lock. A key that is being opened
// or closed is busy, and everyone else who needs that key waits for it, so a
// slow open (a restore from a replica) holds up only its own key and a key is
// never open twice.
package tenant

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// Opener opens the value for key. Its context carries the values of the Get
// that needed it but not its cancellation: a request that gives up must not
// abort a restore halfway.
type Opener[T io.Closer] func(ctx context.Context, key string) (T, error)

var (
	// ErrClosed is returned by Get after Close.
	ErrClosed = errors.New("tenant: pool closed")
	// ErrDenied is the reason Get gives for a key denied without one.
	ErrDenied = errors.New("tenant: key denied")
)

// closeParallelism bounds the closes Close runs at once.
const closeParallelism = 8

// Pool is a set of open values keyed by string. It is safe for concurrent use.
type Pool[T io.Closer] struct {
	open    Opener[T]
	maxOpen int
	log     *slog.Logger

	mu        sync.Mutex
	entries   map[string]*entry[T]
	idle      list.List // unpinned entries, most recently released first
	busy      map[string]*flight
	opening   int
	denied    map[string]error
	closed    bool
	opens     uint64
	evictions uint64
}

type entry[T io.Closer] struct {
	key  string
	val  T
	pins int
	elem *list.Element // in idle while pins == 0
}

// flight is an open or a close of one key in progress.
type flight struct {
	done chan struct{}
	err  error // a failed open's error, for the Gets that waited on it
}

// Option configures a Pool.
type Option func(*options)

type options struct{ log *slog.Logger }

// WithLogger sets where failed closes of evicted values are logged; the
// default is slog.Default().
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.log = l } }

// New returns a pool that opens values with open and closes idle ones beyond
// maxOpen, which must be at least 1.
func New[T io.Closer](maxOpen int, open Opener[T], opts ...Option) *Pool[T] {
	if maxOpen < 1 {
		panic("tenant: maxOpen must be at least 1")
	}
	o := options{log: slog.Default()}
	for _, opt := range opts {
		opt(&o)
	}
	return &Pool[T]{
		open:    open,
		maxOpen: maxOpen,
		log:     o.log,
		entries: map[string]*entry[T]{},
		busy:    map[string]*flight{},
		denied:  map[string]error{},
	}
}

// Get returns the value for key, opening it on first use, and pins it until
// release is called. release may be called more than once.
//
// Hold a pin for one unit of work, a request or a render. An SSE stream that
// pins its tenant for its whole life keeps it from ever being evicted and
// pushes the pool past maxOpen: release between renders and re-acquire, or use
// Do for each render.
//
// ctx bounds the wait for another open or close of key; a cold open started
// by this call runs to completion. Gets that waited on an open that failed
// return its error rather than each trying again.
func (p *Pool[T]) Get(ctx context.Context, key string) (val T, release func(), err error) {
	if err := ctx.Err(); err != nil {
		return val, nil, err
	}
	p.mu.Lock()
	if err := p.settleLocked(ctx, key); err != nil {
		p.mu.Unlock()
		return val, nil, err
	}
	if e := p.entries[key]; e != nil {
		if e.pins == 0 {
			p.idle.Remove(e.elem)
			e.elem = nil
		}
		e.pins++
		p.mu.Unlock()
		return e.val, p.releaser(e), nil
	}

	if err := ctx.Err(); err != nil {
		p.mu.Unlock()
		return val, nil, err
	}
	f := &flight{done: make(chan struct{})}
	p.busy[key] = f
	p.opening++
	victims := p.trimLocked()
	p.mu.Unlock()
	for _, v := range victims {
		go p.retire(v.e, v.f)
	}

	settled := false
	defer func() {
		if !settled { // the opener, or the Close of a refused value, panicked
			p.mu.Lock()
			p.opening--
			p.unbusyLocked(key, f)
			p.mu.Unlock()
		}
	}()
	val, err = p.open(context.WithoutCancel(ctx), key)

	p.mu.Lock()
	if err == nil {
		if reason := p.refusalLocked(key); reason != nil {
			// Closed before busy clears, so whoever denied the key never sees it open.
			p.mu.Unlock()
			if cerr := val.Close(); cerr != nil {
				p.log.Warn("tenant: close value opened for a refused key", "key", key, "error", cerr)
			}
			p.mu.Lock()
			err = reason
		}
	}
	settled = true
	p.opening--
	f.err = err
	p.unbusyLocked(key, f)
	if err != nil {
		p.mu.Unlock()
		var zero T
		return zero, nil, err
	}
	e := &entry[T]{key: key, val: val, pins: 1}
	p.entries[key] = e
	p.opens++
	p.mu.Unlock()
	return val, p.releaser(e), nil
}

// Do runs fn with the value for key pinned for the length of the call.
func (p *Pool[T]) Do(ctx context.Context, key string, fn func(T) error) error {
	v, release, err := p.Get(ctx, key)
	if err != nil {
		return err
	}
	defer release()
	return fn(v)
}

// Evict closes the value for key now, pinned or not, after any open or close
// of key in flight; ctx bounds only that wait. Pins held on the closed value
// go stale: releasing them does nothing. Evicting a key that is not open is
// a no-op.
func (p *Pool[T]) Evict(ctx context.Context, key string) error {
	p.mu.Lock()
	for {
		f, ok := p.busy[key]
		if !ok {
			break
		}
		p.mu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		p.mu.Lock()
	}
	e := p.entries[key]
	if e == nil {
		p.mu.Unlock()
		return nil
	}
	p.removeLocked(e)
	f := &flight{done: make(chan struct{})}
	p.busy[key] = f
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.unbusyLocked(key, f)
		p.mu.Unlock()
	}()

	if err := e.val.Close(); err != nil {
		return fmt.Errorf("tenant: close %s: %w", key, err)
	}
	return nil
}

// Deny makes Get fail with reason (ErrDenied if nil) for key until Allow. An
// open in flight is discarded when it finishes; a value already open stays
// open until Evict. Deny, then Evict, leaves the key closed for good, as an
// erasure or a restore needs.
func (p *Pool[T]) Deny(key string, reason error) {
	if reason == nil {
		reason = ErrDenied
	}
	p.mu.Lock()
	p.denied[key] = reason
	p.mu.Unlock()
}

// Allow lifts a Deny.
func (p *Pool[T]) Allow(key string) {
	p.mu.Lock()
	delete(p.denied, key)
	p.mu.Unlock()
}

// Stats is a snapshot of a pool.
//
// Pinned close to MaxOpen is the condition to alert on: nothing is left to
// evict, and every cold open grows the pool past its cap.
type Stats struct {
	Open      int    // values open
	Pinned    int    // open values with at least one pin
	Opening   int    // opens in flight
	MaxOpen   int    // the soft cap
	Opens     uint64 // successful opens since New
	Evictions uint64 // idle values closed to stay within MaxOpen
}

// Stats reports the pool's occupancy.
func (p *Pool[T]) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		Open:      len(p.entries),
		Pinned:    len(p.entries) - p.idle.Len(),
		Opening:   p.opening,
		MaxOpen:   p.maxOpen,
		Opens:     p.opens,
		Evictions: p.evictions,
	}
}

// Close waits for opens and closes in flight, then closes every value, pinned
// or not, and makes later Gets fail with ErrClosed. It returns the values'
// close errors.
func (p *Pool[T]) Close() error {
	p.mu.Lock()
	p.closed = true
	for len(p.busy) > 0 {
		var f *flight
		for _, f = range p.busy {
			break
		}
		p.mu.Unlock()
		<-f.done
		p.mu.Lock()
	}
	entries := make([]*entry[T], 0, len(p.entries))
	for _, e := range p.entries {
		entries = append(entries, e)
	}
	clear(p.entries)
	p.idle.Init()
	p.mu.Unlock()

	errs := make([]error, len(entries))
	sem := make(chan struct{}, closeParallelism)
	var wg sync.WaitGroup
	for i, e := range entries {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := e.val.Close(); err != nil {
				errs[i] = fmt.Errorf("tenant: close %s: %w", e.key, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// settleLocked waits until key is neither busy nor refused. Called with mu
// held; it is released while waiting and held again on return.
func (p *Pool[T]) settleLocked(ctx context.Context, key string) error {
	for {
		if reason := p.refusalLocked(key); reason != nil {
			return reason
		}
		f, ok := p.busy[key]
		if !ok {
			return nil
		}
		p.mu.Unlock()
		select {
		case <-f.done:
			p.mu.Lock()
			if f.err != nil {
				return f.err
			}
		case <-ctx.Done():
			p.mu.Lock()
			return ctx.Err()
		}
	}
}

func (p *Pool[T]) refusalLocked(key string) error {
	if p.closed {
		return ErrClosed
	}
	return p.denied[key]
}

type victim[T io.Closer] struct {
	e *entry[T]
	f *flight
}

// trimLocked takes idle entries out, least recently released first, until the
// open values and the opens in flight fit maxOpen. Each victim is busy until
// retire has closed it.
func (p *Pool[T]) trimLocked() []victim[T] {
	var victims []victim[T]
	for len(p.entries)+p.opening > p.maxOpen {
		back := p.idle.Back()
		if back == nil {
			break
		}
		e := back.Value.(*entry[T])
		p.removeLocked(e)
		f := &flight{done: make(chan struct{})}
		p.busy[e.key] = f
		p.evictions++
		victims = append(victims, victim[T]{e, f})
	}
	return victims
}

func (p *Pool[T]) retire(e *entry[T], f *flight) {
	defer func() {
		p.mu.Lock()
		p.unbusyLocked(e.key, f)
		p.mu.Unlock()
	}()
	if err := e.val.Close(); err != nil {
		p.log.Warn("tenant: close evicted value", "key", e.key, "error", err)
	}
}

func (p *Pool[T]) removeLocked(e *entry[T]) {
	if e.elem != nil {
		p.idle.Remove(e.elem)
		e.elem = nil
	}
	delete(p.entries, e.key)
}

func (p *Pool[T]) unbusyLocked(key string, f *flight) {
	delete(p.busy, key)
	close(f.done)
}

// releaser returns the release func for one pin on e. It is bound to e, not
// to its key, so a pin outliving an eviction cannot unpin the next value.
func (p *Pool[T]) releaser(e *entry[T]) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.entries[e.key] != e {
				return
			}
			e.pins--
			if e.pins == 0 {
				e.elem = p.idle.PushFront(e)
			}
		})
	}
}
