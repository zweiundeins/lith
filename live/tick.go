package live

import (
	"cmp"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Run renders views until ctx ends. Then it ends every stream at once and
// returns nil when its workers are done; shutdown never waits for a
// stream. It returns an error only when it cannot start.
func (h *Hub) Run(ctx context.Context) error {
	if !h.running.CompareAndSwap(false, true) {
		return errors.New("live: Run called twice")
	}
	defer h.stop()
	defer context.AfterFunc(ctx, h.stop)()

	n := h.workers
	if h.read != nil {
		if m := h.read.Stats().MaxOpenConnections; m > 0 && n >= m {
			n = max(1, m-1)
			h.log.Warn("live: the read pool has too few connections for one per worker", "pool", m, "workers", n)
		}
	}
	ws := make([]*worker, n)
	for i := range ws {
		ws[i] = &worker{h: h, start: make(chan *tick, 1)}
		if h.read == nil {
			continue
		}
		c, err := h.read.Conn(ctx)
		if err != nil {
			for _, w := range ws[:i] {
				w.conn.Close()
			}
			return fmt.Errorf("live: a read connection for a worker: %w", err)
		}
		ws[i].conn = c
	}
	var wg sync.WaitGroup
	for _, w := range ws {
		wg.Go(w.run)
	}
	h.loop(ctx, ws)
	h.stopSampling()
	for _, w := range ws {
		close(w.start)
	}
	wg.Wait()
	for _, w := range ws {
		if w.conn != nil {
			w.conn.Close()
		}
	}
	return nil
}

// loop runs a tick when views are dirty: at once after a quiet spell,
// then at most one per h.tick.
func (h *Hub) loop(ctx context.Context, ws []*worker) {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	t := &tick{ctx: ctx}
	var last time.Time
	for {
		h.mu.Lock()
		next := h.due(time.Now())
		idle := len(h.dirty) == 0
		h.mu.Unlock()
		if idle {
			var due <-chan time.Time
			if !next.IsZero() {
				timer.Reset(time.Until(next))
				due = timer.C
			}
			select {
			case <-ctx.Done():
				return
			case <-h.wake:
			case <-due:
			}
			timer.Stop()
			continue
		}
		if wait := h.tick - time.Since(last); wait > 0 {
			timer.Reset(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		last = time.Now()
		if h.run(t, ws, last) {
			h.measure(time.Since(last))
		}
	}
}

// due marks dirty the views whose Every has passed and returns when the
// next one is due (zero when none is). Hub.mu is held.
func (h *Hub) due(now time.Time) time.Time {
	var next time.Time
	for v := range h.every {
		if !now.Before(v.due) {
			h.mark(v)
			continue
		}
		if next.IsZero() || v.due.Before(next) {
			next = v.due
		}
	}
	return next
}

// tick is the work of one tick, reused from one to the next.
type tick struct {
	ctx    context.Context
	views  []*view
	frames []any
	errs   []error
	next   atomic.Int64 // the queue: index of the next view to render
	wg     sync.WaitGroup

	offers []offer
}

// offer is a new frame for one stream's watch.
type offer struct {
	w *watch
	f *frame
}

// run renders the dirty views and publishes their frames. It reports
// whether there was anything to render.
func (h *Hub) run(t *tick, ws []*worker, start time.Time) bool {
	h.mu.Lock()
	h.due(start)
	t.views = t.views[:0]
	for _, v := range h.dirty {
		v.dirty = false
		if v.dead {
			continue
		}
		if v.every > 0 {
			v.due = start.Add(v.every)
		}
		t.views = append(t.views, v)
	}
	clear(h.dirty)
	h.dirty = h.dirty[:0]
	h.mu.Unlock()
	if len(t.views) == 0 {
		return false
	}

	t.frames = slices.Grow(t.frames[:0], len(t.views))[:len(t.views)]
	t.errs = slices.Grow(t.errs[:0], len(t.views))[:len(t.views)]
	t.next.Store(0)
	k := min(len(ws), len(t.views))
	t.wg.Add(k)
	for _, w := range ws[:k] {
		w.start <- t
	}
	t.wg.Wait()
	h.ticks.Add(1)
	h.publish(t)
	clear(t.views)
	clear(t.frames)
	clear(t.errs)
	return true
}

// publish stores the new frames and offers them to their streams. A
// stream gets all of its offers of the tick at once, so the views that
// changed together go out together. Views that no worker took from the
// queue are dirty again, for the next tick.
func (h *Hub) publish(t *tick) {
	taken := min(int(t.next.Load()), len(t.views))
	h.mu.Lock()
	for i, v := range t.views[:taken] {
		f := t.frames[i]
		if t.errs[i] != nil || f == nil || same(v.frame.Load(), f) {
			continue
		}
		nf := &frame{value: f}
		v.frame.Store(nf)
		for _, w := range v.watches {
			t.offers = append(t.offers, offer{w, nf})
		}
	}
	for _, v := range t.views[taken:] {
		h.mark(v)
	}
	h.mu.Unlock()

	slices.SortFunc(t.offers, func(a, b offer) int { return cmp.Compare(a.w.s.seq, b.w.s.seq) })
	for i := 0; i < len(t.offers); {
		s := t.offers[i].w.s
		s.mu.Lock()
		for ; i < len(t.offers) && t.offers[i].w.s == s; i++ {
			o := t.offers[i]
			if o.w.next != nil {
				h.dropped.Add(1)
			}
			o.w.next = o.f
		}
		s.mu.Unlock()
		s.poke()
	}
	clear(t.offers)
	t.offers = t.offers[:0]

	if t.ctx.Err() != nil {
		return
	}
	for i, err := range t.errs {
		if err != nil {
			h.log.Error("live: render", "view", t.views[i].key, "err", err)
		}
	}
}

// worker renders views from the tick's queue, all in one read
// transaction per tick, on a connection it holds for its lifetime. A
// worker whose transaction does not begin takes no view from the queue.
type worker struct {
	h     *Hub
	conn  *sql.Conn
	start chan *tick
}

func (w *worker) run() {
	for t := range w.start {
		w.render(t)
		t.wg.Done()
	}
}

func (w *worker) render(t *tick) {
	if int(t.next.Load()) >= len(t.views) {
		return
	}
	var tx *sql.Tx
	if w.conn != nil {
		var err error
		if tx, err = w.begin(t.ctx); err != nil {
			if t.ctx.Err() == nil {
				w.h.log.Error("live: beginning a read transaction", "err", err)
			}
			return
		}
	}
	for {
		i := int(t.next.Add(1)) - 1
		if i >= len(t.views) {
			break
		}
		t.frames[i], t.errs[i] = render(t.ctx, t.views[i], tx)
		w.h.renders.Add(1)
	}
	if tx != nil {
		if err := tx.Commit(); err != nil && t.ctx.Err() == nil {
			w.h.log.Error("live: ending a read transaction", "err", err)
		}
	}
}

// begin starts the tick's read transaction. When that fails, the worker
// trades its connection for a fresh one from the pool and tries again.
func (w *worker) begin(ctx context.Context) (*sql.Tx, error) {
	ro := &sql.TxOptions{ReadOnly: true}
	tx, err := w.conn.BeginTx(ctx, ro)
	if err == nil || ctx.Err() != nil {
		return tx, err
	}
	// The pool discards a connection on ErrBadConn; Close would keep it.
	w.conn.Raw(func(any) error { return driver.ErrBadConn })
	c, err2 := w.h.read.Conn(ctx)
	if err2 != nil {
		return nil, errors.Join(err, err2)
	}
	w.conn = c
	return c.BeginTx(ctx, ro)
}

func render(ctx context.Context, v *view, tx *sql.Tx) (f any, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
		}
	}()
	return v.render(ctx, tx)
}

// measure counts an overrun. After one, it samples the duration of every
// tick for h.sampleFor and then logs n, p50, p95, p99 and max.
func (h *Hub) measure(d time.Duration) {
	over := d > h.tick
	if over {
		h.overruns.Add(1)
	}
	h.samplesMu.Lock()
	defer h.samplesMu.Unlock()
	if h.samples == nil {
		if !over {
			return
		}
		h.samples = make([]time.Duration, 0, 64)
		h.report = time.AfterFunc(h.sampleFor, h.reportOverruns)
	}
	h.samples = append(h.samples, d)
}

// stopSampling drops the samples without a report.
func (h *Hub) stopSampling() {
	h.samplesMu.Lock()
	defer h.samplesMu.Unlock()
	if h.report != nil {
		h.report.Stop()
	}
	h.samples = nil
}

// reportOverruns logs the samples. It holds the lock while it logs, so
// stopSampling waits for a report under way.
func (h *Hub) reportOverruns() {
	h.samplesMu.Lock()
	defer h.samplesMu.Unlock()
	s := h.samples
	h.samples = nil
	if len(s) == 0 {
		return
	}
	slices.Sort(s)
	p := func(q int) time.Duration { return s[q*(len(s)-1)/100] }
	h.log.Warn("live: tick overrun", "tick", h.tick, "n", len(s),
		"p50", p(50), "p95", p(95), "p99", p(99), "max", s[len(s)-1])
}
