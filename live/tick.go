package live

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"hash/maphash"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zweiundeins/lith/internal/txctx"
)

// Run renders views until ctx ends. Then it ends every stream: a write
// under way fails at once, and any other stream ends its response
// cleanly, compressed stream and all, within its write timeout. Run
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
		ws[i] = &worker{h: h, start: make(chan *tick, 1), stmts: map[string]*stmt{}}
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
			w.closeStmts()
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
			d := time.Since(last)
			h.measure(d)
			if h.onTick != nil {
				h.onTick(d)
			}
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
	sums   []uint64 // the hashes of the []byte frames
	errs   []error
	deps   []declared
	next   atomic.Int64 // the queue: index of the next view to render
	wg     sync.WaitGroup

	streams []*stream // the streams with offers (stream.offers), in the order of their first
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
		if h.waiting(v) {
			h.deferred.Add(1)
			continue
		}
		v.rendering = true
		t.views = append(t.views, v)
	}
	clear(h.dirty)
	h.dirty = h.dirty[:0]
	var published chan struct{}
	if len(t.views) > 0 {
		published, h.next = h.next, make(chan struct{})
	}
	h.mu.Unlock()
	if len(t.views) == 0 {
		return false
	}

	t.frames = slices.Grow(t.frames[:0], len(t.views))[:len(t.views)]
	t.sums = slices.Grow(t.sums[:0], len(t.views))[:len(t.views)]
	t.errs = slices.Grow(t.errs[:0], len(t.views))[:len(t.views)]
	t.deps = slices.Grow(t.deps[:0], len(t.views))[:len(t.views)]
	t.next.Store(0)
	k := min(len(ws), len(t.views))
	t.wg.Add(k)
	for _, w := range ws[:k] {
		w.start <- t
	}
	t.wg.Wait()
	h.ticks.Add(1)
	h.publish(t)
	close(published)
	clear(t.views)
	clear(t.frames)
	clear(t.errs)
	clear(t.sums)
	clear(t.deps)
	return true
}

// waiting reports whether every watch of v has a frame waiting (Hub.mu held).
// v is marked deferred first, so a pump that takes a slot after a read sees it.
func (h *Hub) waiting(v *view) bool {
	if len(v.watches) == 0 {
		return false
	}
	v.deferred.Store(true)
	for _, w := range v.watches {
		w.s.mu.Lock()
		full := w.next != nil
		w.s.mu.Unlock()
		if !full {
			v.deferred.Store(false)
			return false
		}
	}
	return true
}

// resume marks dirty the deferred views of the frames a pump took, so that
// the next tick renders them as they are now.
func (h *Hub) resume(taken []offer) {
	locked := false
	for _, o := range taken {
		if !o.w.v.deferred.CompareAndSwap(true, false) {
			continue
		}
		if !locked {
			h.mu.Lock()
			locked = true
		}
		h.mark(o.w.v)
	}
	if locked {
		h.mu.Unlock()
		h.poke()
	}
}

// publish offers the new frames to their streams, and a frame that
// repeats the one before to the streams that joined without it. A
// stream gets all of its offers of the tick at once, so the views that
// changed together go out together. Views that no worker took from the
// queue are dirty again, for the next tick, and so is a view without a
// frame for a stream that started watching it during the tick.
func (h *Hub) publish(t *tick) {
	taken := min(int(t.next.Load()), len(t.views))
	var booked int64
	h.mu.Lock()
	for i, v := range t.views[:taken] {
		if d := t.deps[i]; d.changed && !v.dead && h.redepend(v, d.topics, d.seq) {
			h.mark(v)
		}
		joined := v.joined
		v.rendering, v.joined = false, false
		f := t.frames[i]
		if t.errs[i] != nil || f == nil {
			if joined {
				h.mark(v)
			}
			continue
		}
		same := v.unchanged(f, t.sums[i])
		if same && !v.owed {
			continue
		}
		nf := &frame{value: f}
		if !same {
			v.frame, v.size, v.sum = nil, -1, t.sums[i]
			if b, ok := f.([]byte); ok {
				v.size = len(b)
			}
		}
		for _, w := range v.watches {
			if !same || w.owed {
				w.owed = false
				if len(w.s.offers) == 0 {
					t.streams = append(t.streams, w.s)
				}
				w.s.offers = append(w.s.offers, offer{w, nf})
			}
		}
		v.owed = false
		booked += h.keep(v, nf)
	}
	for _, v := range t.views[taken:] {
		v.rendering, v.joined = false, false
		h.mark(v)
	}
	h.mu.Unlock()
	h.sse.Budget.Hold(booked)

	for _, s := range t.streams {
		s.mu.Lock()
		for _, o := range s.offers {
			if o.w.next != nil {
				h.dropped.Add(1)
			}
			o.w.next = o.f
		}
		s.mu.Unlock()
		s.poke()
		clear(s.offers)
		s.offers = s.offers[:0]
	}
	clear(t.streams)
	t.streams = t.streams[:0]

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
	stmts map[string]*stmt // prepared on conn, by their SQL text
	lru   stmtList         // the same statements, the one used last first
	limit limit
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
	// Read before the transaction begins, so that every commit up to it is
	// in the snapshot.
	st := &renderState{seq: w.h.commits.Load()}
	ctx := context.WithValue(t.ctx, renderKey{}, st)
	var tx *sql.Tx
	var q *reads
	if w.conn != nil {
		var err error
		if tx, err = w.begin(t.ctx); err != nil {
			if t.ctx.Err() == nil {
				w.h.log.Error("live: beginning a read transaction", "err", err)
			}
			return
		}
		w.h.readTxs.Add(1)
		q = &reads{w: w, tx: tx}
		ctx = txctx.With(ctx, &txctx.Value{Tx: tx, Pool: w.h.read, Querier: q})
	}
	w.limit.begin(w.h, t.ctx, ctx)
	for {
		i := int(t.next.Add(1)) - 1
		if i >= len(t.views) {
			break
		}
		v := t.views[i]
		st.size.Store(int64(v.size))
		t.frames[i], t.errs[i] = w.limit.render(v, tx)
		// v.deps changes only in publish, after the workers are done.
		t.deps[i] = st.take(v, t.errs[i] != nil)
		if b, ok := t.frames[i].([]byte); ok {
			t.sums[i] = maphash.Bytes(w.h.seed, b)
		}
		if q != nil {
			q.release(false)
		}
		t.views[i].renders.Add(1)
		w.h.renders.Add(1)
	}
	w.limit.end()
	if tx != nil {
		q.release(true)
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
	w.closeStmts()
	// The pool discards a connection on ErrBadConn; Close would keep it.
	w.conn.Raw(func(any) error { return driver.ErrBadConn })
	c, err2 := w.h.read.Conn(ctx)
	if err2 != nil {
		return nil, errors.Join(err, err2)
	}
	w.conn = c
	return c.BeginTx(ctx, ro)
}

// limit ends the ctx of each Render a worker runs once the render
// timeout has passed, and logs a Render still running then. It does so
// with one timer that the worker keeps, and one ctx per tick that lasts
// from one Render to the next until a timeout ends it, so that a Render
// costs no context or timer of its own.
type limit struct {
	h      *Hub
	run    context.Context // Run's ctx
	parent context.Context // the tick's ctx, with the worker's values
	ctx    *renderCtx      // the ctx of the next Render, nil until one needs it
	timer  *time.Timer

	mu  sync.Mutex
	cur *renderCtx // the ctx of the Render under way, until the timer takes it
	key string     // its view's key
	at  time.Time  // when its time is up
}

// renderCtx is the ctx of a worker's Renders: it ends with its parent, or
// when the timer of a Render that runs over ends it. Its Deadline is the
// Render's, and its Err context.DeadlineExceeded after a timeout, as a ctx
// from context.WithTimeout. The ctxs derived from it end with
// context.Canceled and the cause context.DeadlineExceeded.
type renderCtx struct {
	context.Context
	cancel context.CancelCauseFunc
	at     atomic.Int64 // the Render's deadline, in Unix nanoseconds
}

func (c *renderCtx) Deadline() (time.Time, bool) {
	at := time.Unix(0, c.at.Load())
	if d, ok := c.Context.Deadline(); ok && d.Before(at) {
		return d, true
	}
	return at, true
}

func (c *renderCtx) Err() error {
	err := c.Context.Err()
	if err != nil && errors.Is(context.Cause(c.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return err
}

// begin starts a tick whose Renders take ctx, under Run's ctx run.
func (l *limit) begin(h *Hub, run, ctx context.Context) {
	if l.h == nil {
		// Before the timer exists, which reads them.
		l.h, l.run = h, run
	}
	l.parent = ctx
}

// end ends the tick's ctx.
func (l *limit) end() {
	if l.ctx != nil {
		l.ctx.cancel(nil)
		l.ctx = nil
	}
	l.parent = nil
}

// render runs v's Render, whose ctx ends after the render timeout.
func (l *limit) render(v *view, tx *sql.Tx) (any, error) {
	c := l.ctx
	if c == nil {
		ctx, cancel := context.WithCancelCause(l.parent)
		c = &renderCtx{Context: ctx, cancel: cancel}
		l.ctx = c
	}
	d := l.h.renderTimeout
	at := time.Now().Add(d)
	c.at.Store(at.UnixNano())
	l.mu.Lock()
	l.cur, l.key, l.at = c, v.key, at
	l.mu.Unlock()
	if l.timer == nil {
		l.timer = time.AfterFunc(d, l.fire)
	} else {
		l.timer.Reset(d)
	}
	f, err := render(c, v, tx)
	l.mu.Lock()
	taken := l.cur == nil
	l.cur = nil
	l.mu.Unlock()
	l.timer.Stop()
	switch {
	case taken:
		// The timer ended c.
		l.ctx = nil
	case !time.Now().Before(at):
		// The Render returned as its time ran out, before the timer took it.
		l.overrun(v.key)
	}
	return f, err
}

// fire logs the Render under way once its time is up, and ends its ctx. A
// timer that fired for a Render that has returned since finds the next
// one's time not up, and leaves it alone.
func (l *limit) fire() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c := l.cur; c != nil && !time.Now().Before(l.at) {
		l.cur = nil
		l.overrun(l.key)
		c.cancel(context.DeadlineExceeded)
	}
}

func (l *limit) overrun(key string) {
	if l.run.Err() == nil {
		l.h.log.Warn("live: render overrun", "view", key, "timeout", l.h.renderTimeout)
	}
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
