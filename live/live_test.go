package live

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
)

func counterView(key string, renders *atomic.Int64) View {
	return text(key, []string{"counter"}, func(ctx context.Context, tx *sql.Tx) (string, error) {
		if renders != nil {
			renders.Add(1)
		}
		n, err := count(ctx, tx)
		return strconv.Itoa(n), err
	})
}

// A burst of commits after a quiet spell renders at once, then once more
// for whatever came during that tick: never once per commit. The hub sees
// a commit only through Changed, so here a commit is an increment.
func TestBurstOfCommitsRendersAtMostTwice(t *testing.T) {
	h := start(t, Options{Tick: 50 * time.Millisecond})
	var state, renders atomic.Int64
	v := text("n", []string{"counter"}, func(context.Context, *sql.Tx) (string, error) {
		renders.Add(1)
		return strconv.Itoa(int(state.Load())), nil
	})
	c := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the first frame", func() bool { return c.has(`<p id="n">0</p>`) })
	time.Sleep(100 * time.Millisecond)
	renders.Store(0)

	begin := time.Now()
	for range 100 {
		state.Add(1)
		h.Changed([]string{"counter"})
	}
	if took := time.Since(begin); took > 10*time.Millisecond {
		t.Fatalf("100 commits took %v", took)
	}
	waitFor(t, "the last commit", func() bool { return c.has(`<p id="n">100</p>`) })
	time.Sleep(120 * time.Millisecond)
	if n := renders.Load(); n > 2 {
		t.Fatalf("%d renders for 100 commits", n)
	}
}

// Every stream on a view gets the same frame from one render.
func TestManyStreamsShareOneRender(t *testing.T) {
	write, read := testDB(t, 4)
	h := start(t, Options{Read: read, Workers: 2})
	var renders atomic.Int64
	v := counterView("n", &renders)
	cs := make([]*client, 50)
	for i := range cs {
		cs[i] = serve(h, Spec{Views: []View{v}}, "")
	}
	for _, c := range cs {
		waitFor(t, "the first frame", func() bool { return c.has(`<p id="n">0</p>`) })
	}
	increment(t, write)
	h.Changed([]string{"counter"})
	for _, c := range cs {
		waitFor(t, "the second frame", func() bool { return c.has(`<p id="n">1</p>`) })
	}
	s := h.Stats()
	if renders.Load() != 2 || s.Renders != 2 || s.Ticks != 2 || s.FramesSent != 100 {
		t.Fatalf("%d renders, stats %+v", renders.Load(), s)
	}
	for _, c := range cs {
		if p := c.patches(); len(p) != 2 {
			t.Fatalf("a stream got %q", p)
		}
	}
}

// A tick's views are shared out among the workers, and each worker
// renders all of its views in one read transaction, which ends with the
// tick: one snapshot per worker, even while commits land.
func TestOneReadTransactionPerWorkerPerTick(t *testing.T) {
	write, read := testDB(t, 8)
	const workers, views = 4, 40
	h := start(t, Options{Read: read, Workers: workers})
	var mu sync.Mutex
	seen := map[*sql.Tx][]int{} // what each transaction read
	vs := make([]View, views)
	for i := range vs {
		vs[i] = text(fmt.Sprint("v", i), []string{"counter"}, func(ctx context.Context, tx *sql.Tx) (string, error) {
			n, err := count(ctx, tx)
			time.Sleep(time.Millisecond)
			mu.Lock()
			seen[tx] = append(seen[tx], n)
			mu.Unlock()
			return strconv.Itoa(n), err
		})
	}
	serve(h, Spec{Views: vs}, "")
	waitFor(t, "the first tick", func() bool { s := h.Stats(); return s.Renders == views && s.Ticks > 0 })
	if n := h.Stats().Ticks; n != 1 {
		t.Fatalf("the views took %d ticks", n)
	}
	mu.Lock()
	clear(seen)
	mu.Unlock()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				write.Exec(`update counter set n = n + 1`)
			}
		}
	})
	h.Changed([]string{"counter"})
	waitFor(t, "the second tick", func() bool { s := h.Stats(); return s.Renders == 2*views && s.Ticks > 1 })
	close(stop)
	wg.Wait()

	if n := h.Stats().Ticks; n != 2 {
		t.Fatalf("%d ticks", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || len(seen) > workers {
		t.Fatalf("%d read transactions for %d workers", len(seen), workers)
	}
	for tx, ns := range seen {
		for _, n := range ns {
			if n != ns[0] {
				t.Fatalf("one transaction read %v", ns)
			}
		}
		if err := tx.QueryRow(`select 1`).Scan(new(int)); !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("a read transaction outlived its tick: %v", err)
		}
	}
}

// A Render's ctx carries its transaction, so a store that reads through
// sqlite.DB.View with that ctx sees the render's snapshot, even when a
// commit lands while the view renders.
func TestStoreInRenderJoinsItsSnapshot(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "test.db"), sqlite.Options{Synchronous: "NORMAL"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.W.Exec(`create table counter (name text primary key, n integer not null); insert into counter values ('a', 0)`); err != nil {
		t.Fatal(err)
	}
	store := func(ctx context.Context) (n int, err error) {
		err = db.View(ctx, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `select n from counter where name = 'a'`).Scan(&n)
		})
		return n, err
	}
	type seen struct {
		carried     bool
		render, got int
	}
	results := make(chan seen, 1)
	h := start(t, Options{Read: db.R, Workers: 2})
	v := text("v", nil, func(ctx context.Context, tx *sql.Tx) (string, error) {
		var s seen
		got, ok := sqlite.TxFrom(ctx)
		s.carried = ok && got == tx
		var err error
		if s.render, err = count(ctx, tx); err != nil {
			return "", err
		}
		if _, err := db.W.Exec(`update counter set n = n + 1 where name = 'a'`); err != nil {
			return "", err
		}
		if s.got, err = store(ctx); err != nil {
			return "", err
		}
		results <- s
		return "v", nil
	})
	serve(h, Spec{Views: []View{v}}, "")
	select {
	case s := <-results:
		if !s.carried {
			t.Error("the Render's ctx does not carry its transaction")
		}
		if s.got != s.render {
			t.Errorf("the store read %d, the render %d: it did not join the render's snapshot", s.got, s.render)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the view did not render")
	}
}

// Stats count the read transactions the workers begin, at most one per
// worker and tick, and the renders of each view watched now.
func TestStatsCountReadTransactionsAndRendersByKey(t *testing.T) {
	_, read := testDB(t, 4)
	h := start(t, Options{Read: read, Workers: 2, Tick: 5 * time.Millisecond})
	a := text("a", []string{"a"}, func(context.Context, *sql.Tx) (string, error) { return "a", nil })
	b := text("b", []string{"b"}, func(context.Context, *sql.Tx) (string, error) { return "b", nil })
	c := serve(h, Spec{ID: "tab", Views: []View{a, b}}, "")
	waitFor(t, "the first frames", func() bool { return len(c.patches()) == 2 })
	for i := int64(2); i <= 3; i++ {
		h.Changed([]string{"a"})
		waitFor(t, "a render of a", func() bool { return h.Stats().RendersByKey["a"] == i })
	}
	waitFor(t, "the third tick", func() bool { return h.Stats().Ticks == 3 })
	s := h.Stats()
	if want := map[string]int64{"a": 3, "b": 1}; !maps.Equal(s.RendersByKey, want) {
		t.Fatalf("renders by key %v, want %v", s.RendersByKey, want)
	}
	if s.ReadTxs < s.Ticks || s.ReadTxs > 2*s.Ticks {
		t.Fatalf("%d read transactions in %d ticks of two workers", s.ReadTxs, s.Ticks)
	}
	h.Unwatch("tab", "a")
	if _, ok := h.Stats().RendersByKey["a"]; ok {
		t.Fatal("a view nobody watches is still counted")
	}

	h = start(t, Options{Tick: 5 * time.Millisecond})
	c = serve(h, Spec{Views: []View{a}}, "")
	waitFor(t, "the frame", func() bool { return c.has(`<p id="a">a</p>`) })
	if s := h.Stats(); s.ReadTxs != 0 || s.RendersByKey["a"] != 1 {
		t.Fatalf("without a read pool: %+v", s)
	}
}

// sqlite.QuerierFrom gives a Render its worker's prepared statements: each
// SQL text is prepared once on the worker's connection, whatever the
// number of renders and ticks, and runs in the render's snapshot. A query
// that runs again while its rows are open, as in a nested loop, runs on a
// statement of its own.
func TestRenderQuerierPreparesOnce(t *testing.T) {
	write, read, prepares := countingDB(t, 4)
	l := &logs{}
	h := start(t, Options{Read: read, Workers: 1, Tick: 2 * time.Millisecond, Log: l.logger()})
	const one, names = `select n from counter where name = ?`, `select name from counter`
	render := func(ctx context.Context, tx *sql.Tx) (string, error) {
		q, ok := sqlite.QuerierFrom(ctx)
		if !ok {
			return "", errors.New("no Querier in the ctx")
		}
		var before, after int
		if err := q.QueryRowContext(ctx, one, "a").Scan(&before); err != nil {
			return "", err
		}
		if _, err := write.Exec(`update counter set n = n + 1 where name = 'b'`); err != nil {
			return "", err
		}
		if err := q.QueryRowContext(ctx, one, "a").Scan(&after); err != nil {
			return "", err
		}
		if after != before {
			return "", fmt.Errorf("the Querier left the render's snapshot: %d, then %d", before, after)
		}
		rows, err := q.QueryContext(ctx, names)
		if err != nil {
			return "", err
		}
		defer rows.Close()
		pairs := 0
		for rows.Next() {
			inner, err := q.QueryContext(ctx, names)
			if err != nil {
				return "", err
			}
			for inner.Next() {
				pairs++
			}
			if err := inner.Close(); err != nil {
				return "", err
			}
		}
		if err := rows.Err(); err != nil {
			return "", err
		}
		return fmt.Sprintf("%d/%d", before, pairs), nil
	}
	if _, err := write.Exec(`insert into counter values ('b', 0)`); err != nil {
		t.Fatal(err)
	}
	c := serve(h, Spec{Views: []View{text("n", []string{"counter"}, render)}}, "")
	waitFor(t, "the first frame", func() bool { return c.has(`<p id="n">0/4</p>`) })
	for i := 1; i <= 5; i++ {
		increment(t, write)
		h.Changed([]string{"counter"})
		waitFor(t, "the next frame", func() bool { return c.has(fmt.Sprintf(`<p id="n">%d/4</p>`, i)) })
	}
	if strings.Contains(l.String(), "render") {
		t.Fatalf("logged %s", l.String())
	}
	if n := prepares.Load(); n != 2 {
		t.Fatalf("%d statements prepared for two queries in six ticks, want 2", n)
	}
}

// A row that a Render leaves unscanned does not keep its statement busy,
// nor the worker's snapshot from moving on; a Querier kept beyond its
// tick fails.
func TestRenderQuerierAfterTheRender(t *testing.T) {
	write, read, _ := countingDB(t, 4)
	h := start(t, Options{Read: read, Workers: 1, Tick: 2 * time.Millisecond})
	kept := make(chan sqlite.Querier, 1)
	v := text("n", []string{"counter"}, func(ctx context.Context, tx *sql.Tx) (string, error) {
		n, err := count(ctx, tx)
		if err != nil {
			return "", err
		}
		q, _ := sqlite.QuerierFrom(ctx)
		q.QueryRowContext(context.Background(), `select n from counter where name = ?`, "a")
		select {
		case kept <- q:
		default:
		}
		return strconv.Itoa(n), nil
	})
	c := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the first frame", func() bool { return c.has(`<p id="n">0</p>`) })
	for i := 1; i <= 3; i++ {
		increment(t, write)
		h.Changed([]string{"counter"})
		waitFor(t, "a frame of the new snapshot", func() bool { return c.has(fmt.Sprintf(`<p id="n">%d</p>`, i)) })
	}
	var n int
	if err := (<-kept).QueryRowContext(context.Background(), `select n from counter where name = ?`, "a").Scan(&n); err == nil {
		t.Fatal("a Querier read after its tick")
	}
}

// renderOnce runs render in a view's Render, with the Render's Querier
// and worker, and returns its result once the Render is done.
func renderOnce[T any](t *testing.T, read *sql.DB, render func(context.Context, sqlite.Querier, *worker) (T, error)) T {
	t.Helper()
	h := start(t, Options{Read: read, Workers: 1})
	type result struct {
		v   T
		err error
	}
	results := make(chan result, 1)
	v := text("v", nil, func(ctx context.Context, _ *sql.Tx) (string, error) {
		q, _ := sqlite.QuerierFrom(ctx)
		r, err := render(ctx, q, q.(*reads).w)
		results <- result{r, err}
		return "v", err
	})
	c := serve(h, Spec{Views: []View{v}}, "")
	var r result
	select {
	case r = <-results:
	case <-time.After(5 * time.Second):
		t.Fatal("the view did not render")
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	waitFor(t, "the end of the render", func() bool { return c.has(`<p id="v">v</p>`) })
	return r.v
}

func closed(s *sql.Stmt) bool {
	_, err := s.ExecContext(context.Background())
	return err != nil && strings.Contains(err.Error(), "closed")
}

// A worker's statement cache keeps the statements used last: SQL built at
// runtime pushes out, and closes, the ones used longest ago, and a
// statement that comes later still gets a place.
func TestRenderQuerierKeepsTheLastUsed(t *testing.T) {
	_, read := testDB(t, 4)
	const hot = `select n from counter where name = ?`
	built := func(i int) string { return fmt.Sprintf(`select n + %d from counter where name = ?`, i) }
	first := renderOnce(t, read, func(ctx context.Context, q sqlite.Querier, w *worker) (*sql.Stmt, error) {
		var first *sql.Stmt
		for i := range maxStmts + 10 {
			var n int
			if err := q.QueryRowContext(ctx, built(i), "a").Scan(&n); err != nil {
				return nil, err
			}
			if i == 0 {
				first = w.stmts[built(0)].s
			}
			if err := q.QueryRowContext(ctx, hot, "a").Scan(&n); err != nil {
				return nil, err
			}
		}
		switch {
		case len(w.stmts) != maxStmts:
			return nil, fmt.Errorf("%d statements cached, want %d", len(w.stmts), maxStmts)
		case w.stmts[hot] == nil:
			return nil, errors.New("the statement used all along has no place")
		case w.stmts[built(maxStmts+9)] == nil:
			return nil, errors.New("the statement used last has no place")
		case w.stmts[built(0)] != nil:
			return nil, errors.New("the statement used longest ago is still cached")
		}
		return first, nil
	})
	if !closed(first) {
		t.Fatal("a statement pushed out is still open after its Render")
	}
}

// A statement pushed out of a worker's cache while the Render still reads
// it, its rows open or its row not scanned yet, stays open until the
// Render is done.
func TestRenderQuerierEvictionUnderAnOpenQuery(t *testing.T) {
	write, read := testDB(t, 4)
	if _, err := write.Exec(`insert into counter values ('b', 1), ('c', 2)`); err != nil {
		t.Fatal(err)
	}
	const all, one = `select name from counter order by name`, `select n from counter where name = ?`
	type result struct {
		names     []string
		found     int
		pushedOut []*sql.Stmt
	}
	r := renderOnce(t, read, func(ctx context.Context, q sqlite.Querier, w *worker) (r result, err error) {
		rows, err := q.QueryContext(ctx, all)
		if err != nil {
			return r, err
		}
		defer rows.Close()
		row := q.QueryRowContext(ctx, one, "c")
		r.pushedOut = []*sql.Stmt{w.stmts[all].s, w.stmts[one].s}
		if !rows.Next() {
			return r, errors.New("no first row")
		}
		var name string
		if err := rows.Scan(&name); err != nil {
			return r, err
		}
		r.names = append(r.names, name)
		// More SQL texts built at runtime than the cache holds.
		for i := 1; i <= maxStmts+10; i++ {
			var n int
			if err := q.QueryRowContext(ctx, fmt.Sprintf(`select count(*) from counter limit %d`, i)).Scan(&n); err != nil {
				return r, err
			}
		}
		if w.stmts[all] != nil || w.stmts[one] != nil {
			return r, errors.New("the statements read from are still cached")
		}
		if err := row.Scan(&r.found); err != nil {
			return r, err
		}
		for rows.Next() {
			if err := rows.Scan(&name); err != nil {
				return r, err
			}
			r.names = append(r.names, name)
		}
		return r, rows.Err()
	})
	if r.found != 2 || !slices.Equal(r.names, []string{"a", "b", "c"}) {
		t.Fatalf("read %v and %d", r.names, r.found)
	}
	for _, s := range r.pushedOut {
		if !closed(s) {
			t.Fatal("a statement pushed out is still open after its Render")
		}
	}
}

// slowViews returns views v0, v1, ... that each take a millisecond to
// render their key.
func slowViews(n int) []View {
	vs := make([]View, n)
	for i := range vs {
		key := fmt.Sprint("v", i)
		vs[i] = text(key, nil, func(context.Context, *sql.Tx) (string, error) {
			time.Sleep(time.Millisecond)
			return key, nil
		})
	}
	return vs
}

// A worker whose read transaction does not begin leaves the tick's views
// to the others.
func TestWorkerWithoutTransactionLeavesViewsToOthers(t *testing.T) {
	db := newFakeDB(t, 5, 1, 4)
	h := start(t, Options{Read: db.DB, Workers: 4})
	c := serve(h, Spec{Views: slowViews(40)}, "")
	waitFor(t, "every view", func() bool { return len(c.patches()) == 40 })
	if n := h.Stats().Ticks; n != 1 {
		t.Fatalf("the views took %d ticks", n)
	}
}

// A worker whose transaction does not begin trades its connection for a
// new one, not for the same one back from the pool.
func TestWorkerReplacesBrokenConnection(t *testing.T) {
	db := newFakeDB(t, 2, 1, 0)
	h := start(t, Options{Read: db.DB, Workers: 1, Tick: 5 * time.Millisecond})
	c := serve(h, Spec{Views: slowViews(1)}, "")
	waitFor(t, "the view", func() bool { return c.has(`<p id="v0">v0</p>`) })
	if n := h.Stats().Ticks; n != 1 {
		t.Fatalf("the view took %d ticks", n)
	}
}

// Views that no worker could render, since no read transaction began,
// render in a later tick without a commit: a stream already watching
// them gets them once reads work again.
func TestViewsRenderOnceReadsWork(t *testing.T) {
	db := newFakeDB(t, 3, 0, 0)
	h := start(t, Options{Read: db.DB, Workers: 2, Tick: 5 * time.Millisecond})
	waitFor(t, "the workers' connections", func() bool { return db.opened.Load() == 2 })
	db.down.Store(true)
	c := serve(h, Spec{Views: slowViews(4)}, "")
	waitFor(t, "a tick", func() bool { return h.Stats().Ticks > 0 })
	db.down.Store(false)
	waitFor(t, "every view", func() bool { return len(c.patches()) == 4 })
}

// A client that stops reading skips frames and loses its stream at the
// write deadline. Meanwhile the others get every frame: the stalled
// stream holds neither a lock nor a compression slot.
func TestStalledClientHoldsUpNoOne(t *testing.T) {
	write, read := testDB(t, 4)
	h := start(t, Options{Read: read, Workers: 2, Tick: 5 * time.Millisecond,
		SSE: sse.Options{Compress: make(chan struct{}, 1), WriteTimeout: time.Second}})
	v := counterView("n", nil)
	slow := serve(h, Spec{Views: []View{v}}, "zstd")
	fast := serve(h, Spec{Views: []View{v}}, "zstd")
	got := func(c *client, html string) func() bool {
		return func() bool { return strings.Contains(string(unzstd(t, c.bytes())), html) }
	}
	waitFor(t, "the first frames", func() bool { return got(slow, `<p id="n">0</p>`)() && got(fast, `<p id="n">0</p>`)() })

	slow.stall()
	for i := 1; i <= 20; i++ {
		increment(t, write)
		h.Changed([]string{"counter"})
		waitFor(t, "the healthy stream", got(fast, fmt.Sprintf(`<p id="n">%d</p>`, i)))
	}
	if h.Stats().FramesDropped == 0 {
		t.Fatal("the stalled stream dropped no frames")
	}
	select {
	case err := <-slow.done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("the stalled stream ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled stream outlived its write deadline")
	}
	increment(t, write)
	h.Changed([]string{"counter"})
	waitFor(t, "the healthy stream", got(fast, `<p id="n">21</p>`))
}

// leavingClient goes away during a write once leaving is set: like
// net/http, it ends the request's context before the write fails.
type leavingClient struct {
	*client
	leaving atomic.Bool
}

func (c *leavingClient) Write(p []byte) (int, error) {
	if c.leaving.Load() {
		c.cancel()
		return 0, errors.New("write: broken pipe")
	}
	return c.client.Write(p)
}

// A client that leaves during a write is a client that leaves: Serve
// returns nil, not the write's error.
func TestClientLeavingMidWrite(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	var n atomic.Int64
	v := text("n", []string{"n"}, func(context.Context, *sql.Tx) (string, error) {
		return strconv.Itoa(int(n.Load())), nil
	})
	var lc *leavingClient
	c := serveThrough(h, Spec{Views: []View{v}}, "", func(c *client) http.ResponseWriter {
		lc = &leavingClient{client: c}
		return lc
	})
	waitFor(t, "the first frame", func() bool { return c.has(`<p id="n">0</p>`) })
	lc.leaving.Store(true)
	n.Store(1)
	h.Changed([]string{"n"})
	if err := c.returned(t); err != nil {
		t.Fatalf("Serve returned %v", err)
	}
}

func TestMaxStreams(t *testing.T) {
	h := start(t, Options{MaxStreams: 2})
	v := text("a", nil, func(context.Context, *sql.Tx) (string, error) { return "a", nil })
	a := serve(h, Spec{Views: []View{v}}, "")
	b := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "two streams", func() bool { return a.has(`<p id="a">a</p>`) && b.has(`<p id="a">a</p>`) })
	c := serve(h, Spec{Views: []View{v}}, "")
	select {
	case err := <-c.done:
		if !errors.Is(err, ErrFull) || c.status() != http.StatusServiceUnavailable {
			t.Fatalf("a third stream: %v, status %d", err, c.status())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a third stream was served")
	}
	a.leave(t)
	d := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "a stream in the freed place", func() bool { return d.has(`<p id="a">a</p>`) })
}

func TestWatchAndUnwatch(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	var n atomic.Int64
	panel := text("panel", []string{"panel"}, func(context.Context, *sql.Tx) (string, error) {
		return strconv.Itoa(int(n.Load())), nil
	})
	page := text("page", nil, func(context.Context, *sql.Tx) (string, error) { return "page", nil })
	c := serve(h, Spec{ID: "tab1", Views: []View{page}}, "")
	waitFor(t, "the page", func() bool { return c.has(`<p id="page">page</p>`) })

	if h.Watch("tab2", panel) {
		t.Fatal("watched for a stream that is not open")
	}
	if !h.Watch("tab1", panel) {
		t.Fatal("could not watch")
	}
	waitFor(t, "the panel", func() bool { return c.has(`<p id="panel">0</p>`) })
	n.Store(1)
	h.Changed([]string{"panel"})
	waitFor(t, "the panel again", func() bool { return c.has(`<p id="panel">1</p>`) })

	if !h.Unwatch("tab1", "panel") || h.Unwatch("tab1", "panel") {
		t.Fatal("Unwatch")
	}
	if h.Count("panel") != 0 || h.Stats().Views != 1 {
		t.Fatalf("the panel is still there: %+v", h.Stats())
	}
	n.Store(2)
	h.Changed([]string{"panel"})
	time.Sleep(50 * time.Millisecond)
	if c.has(`<p id="panel">2</p>`) {
		t.Fatal("got the panel after Unwatch")
	}

	// Watching it again starts from its current state.
	h.Watch("tab1", panel)
	waitFor(t, "the panel after watching again", func() bool { return c.has(`<p id="panel">2</p>`) })
}

// A page's command to watch a view can arrive before its stream opens:
// the stream that opens with the ID watches the view from the start.
func TestWatchBeforeTheStreamOpens(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	panel := text("panel", nil, func(context.Context, *sql.Tx) (string, error) { return "panel", nil })
	page := text("page", nil, func(context.Context, *sql.Tx) (string, error) { return "page", nil })
	if h.Watch("tab1", panel) {
		t.Fatal("Watch reported a stream that is not open")
	}
	if h.Watch("", panel) {
		t.Fatal("Watch reported a stream without an ID")
	}
	c := serve(h, Spec{ID: "tab1", Views: []View{page}}, "")
	waitFor(t, "the pending view", func() bool { return c.has(`<p id="panel">panel</p>`) })
	if p := c.patches(); len(p) != 2 || p[0] != `<p id="page">page</p>` {
		t.Fatalf("got %q, want the page, then the panel", p)
	}
	if n := h.Count("panel"); n != 1 {
		t.Fatalf("%d streams on the panel", n)
	}
	// Once the stream took the view, nothing waits for a later one.
	c.leave(t)
	again := serve(h, Spec{ID: "tab1", Views: []View{page}}, "")
	waitFor(t, "the page", func() bool { return again.has(`<p id="page">page</p>`) })
	time.Sleep(30 * time.Millisecond)
	if again.has(`<p id="panel">panel</p>`) {
		t.Fatal("a second stream with the ID got the view")
	}
}

// A pending view waits PendingWatch at most, Unwatch drops it, and a
// negative PendingWatch keeps none.
func TestPendingWatchEnds(t *testing.T) {
	panel := text("panel", nil, func(context.Context, *sql.Tx) (string, error) { return "panel", nil })
	page := text("page", nil, func(context.Context, *sql.Tx) (string, error) { return "page", nil })
	opened := func(h *Hub, id string) *client {
		c := serve(h, Spec{ID: id, Views: []View{page}}, "")
		waitFor(t, "the page", func() bool { return c.has(`<p id="page">page</p>`) })
		time.Sleep(30 * time.Millisecond)
		return c
	}
	h := start(t, Options{Tick: 5 * time.Millisecond, PendingWatch: 50 * time.Millisecond})
	h.Watch("late", panel)
	h.Watch("dropped", panel)
	if !h.Unwatch("dropped", "panel") || h.Unwatch("dropped", "panel") {
		t.Fatal("Unwatch of a pending view")
	}
	time.Sleep(100 * time.Millisecond)
	if c := opened(h, "late"); c.has(`<p id="panel">panel</p>`) {
		t.Fatal("a view waited longer than PendingWatch")
	}
	if c := opened(h, "dropped"); c.has(`<p id="panel">panel</p>`) {
		t.Fatal("an unwatched view waited")
	}

	h = start(t, Options{Tick: 5 * time.Millisecond, MaxStreams: 3})
	for _, id := range []string{"x", "y", "z", "w"} {
		h.Watch(id, panel)
	}
	if c := opened(h, "w"); c.has(`<p id="panel">panel</p>`) {
		t.Fatal("views waited for more IDs than MaxStreams")
	}
	if c := opened(h, "x"); !c.has(`<p id="panel">panel</p>`) {
		t.Fatal("a view did not wait")
	}

	h = start(t, Options{Tick: 5 * time.Millisecond, PendingWatch: -1})
	h.Watch("tab", panel)
	if c := opened(h, "tab"); c.has(`<p id="panel">panel</p>`) {
		t.Fatal("a view waited with a negative PendingWatch")
	}
}

// When the hub stops, every stream ends at once, even one stuck in a
// write to a client that stopped reading.
func TestShutdownWaitsForNoStream(t *testing.T) {
	_, read := testDB(t, 4)
	h := New(Options{Read: read, Workers: 2, Log: slog.New(slog.DiscardHandler), SSE: sse.Options{WriteTimeout: time.Minute}})
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- h.Run(ctx) }()
	var n atomic.Int64
	v := text("n", []string{"n"}, func(context.Context, *sql.Tx) (string, error) {
		return strconv.Itoa(int(n.Load())), nil
	})
	cs := make([]*client, 10)
	for i := range cs {
		cs[i] = serve(h, Spec{Views: []View{v}}, "")
		waitFor(t, "the first frame", func() bool { return cs[i].has(`<p id="n">0</p>`) })
	}
	stuck := cs[0]
	stuck.stall()
	n.Store(1)
	h.Changed([]string{"n"})
	<-stuck.blocked

	begin := time.Now()
	cancel()
	select {
	case err := <-ran:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	for _, c := range cs {
		select {
		case err := <-c.done:
			if err != nil {
				t.Fatalf("Serve returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a stream outlived the hub")
		}
	}
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("shutdown took %v", d)
	}
	late := serve(h, Spec{Views: []View{v}}, "")
	if err := <-late.done; !errors.Is(err, ErrStopped) || late.status() != http.StatusServiceUnavailable {
		t.Fatalf("a stream after the hub stopped: %v, status %d", err, late.status())
	}
}

// stoppingClient stops the hub while the stream flushes its headers, and
// the flush fails as the stream ends with it.
type stoppingClient struct {
	*client
	h *Hub
}

func (c *stoppingClient) FlushError() error {
	c.h.stop()
	return context.Canceled
}

// A stream that opens while the hub stops gets a plain 503, without the
// Content-Encoding the stream had set.
func TestStopWhileOpening(t *testing.T) {
	h := New(Options{Log: slog.New(slog.DiscardHandler)})
	v := text("v", nil, func(context.Context, *sql.Tx) (string, error) { return "v", nil })
	c := serveThrough(h, Spec{Views: []View{v}}, "zstd", func(c *client) http.ResponseWriter {
		return &stoppingClient{client: c, h: h}
	})
	if err := c.returned(t); !errors.Is(err, ErrStopped) || c.status() != http.StatusServiceUnavailable {
		t.Fatalf("Serve returned %v, status %d", err, c.status())
	}
	if e := c.header.Get("Content-Encoding"); e != "" {
		t.Fatalf("the 503 says Content-Encoding %q", e)
	}
}

// A commit after a quiet spell renders at once; the next tick waits for
// the tick interval.
func TestLeadingEdgeThenTick(t *testing.T) {
	h := start(t, Options{Tick: 200 * time.Millisecond})
	var mu sync.Mutex
	var at []time.Time
	v := View{Key: "v", Topics: []string{"t"}, Render: func(context.Context, *sql.Tx) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		at = append(at, time.Now())
		return sse.PatchElements(fmt.Appendf(nil, `<p id="v">%d</p>`, len(at))), nil
	}}
	renders := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(at)
	}
	serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the first render", func() bool { return renders() == 1 })
	time.Sleep(250 * time.Millisecond)

	changed := time.Now()
	h.Changed([]string{"t"})
	waitFor(t, "the second render", func() bool { return renders() == 2 })
	h.Changed([]string{"t"})
	waitFor(t, "the third render", func() bool { return renders() == 3 })
	mu.Lock()
	defer mu.Unlock()
	if d := at[1].Sub(changed); d > 100*time.Millisecond {
		t.Fatalf("rendered %v after a commit that followed a quiet spell", d)
	}
	if d := at[2].Sub(at[1]); d < 180*time.Millisecond {
		t.Fatalf("two ticks %v apart", d)
	}
}

func TestEvery(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	var n atomic.Int64
	v := View{Key: "clock", Every: 30 * time.Millisecond, Render: func(context.Context, *sql.Tx) (any, error) {
		return sse.PatchElements(fmt.Appendf(nil, `<p id="clock">%d</p>`, n.Add(1))), nil
	}}
	c := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the first frame", func() bool { return c.has(`<p id="clock">1</p>`) })
	time.Sleep(300 * time.Millisecond)
	if got := n.Load(); got < 6 || got > 13 {
		t.Fatalf("%d renders in 300 ms, every 30 ms", got)
	}
	waitFor(t, "a later frame", func() bool { return c.has(`<p id="clock">6</p>`) })
}

func TestPresence(t *testing.T) {
	var h *Hub
	h = start(t, Options{Tick: 5 * time.Millisecond})
	v := text("here", nil, func(context.Context, *sql.Tx) (string, error) { return strconv.Itoa(h.Count("here")), nil })
	v.Presence = true
	last := func(c *client) string {
		p := c.patches()
		if len(p) == 0 {
			return ""
		}
		return p[len(p)-1]
	}
	a := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "one present", func() bool { return last(a) == `<p id="here">1</p>` })
	b := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "two present", func() bool { return last(a) == `<p id="here">2</p>` && last(b) == `<p id="here">2</p>` })
	b.leave(t)
	waitFor(t, "one left", func() bool { return last(a) == `<p id="here">1</p>` })
	a.leave(t)
	if s := h.Stats(); s.Views != 0 || s.Streams != 0 {
		t.Fatalf("left behind: %+v", s)
	}
}

// A failed render, or one that panics, is logged and keeps the frame the
// page has.
func TestRenderFailureKeepsFrame(t *testing.T) {
	l := &logs{}
	h := start(t, Options{Tick: 5 * time.Millisecond, Log: l.logger()})
	var calls atomic.Int64
	v := View{Key: "v", Topics: []string{"t"}, Render: func(context.Context, *sql.Tx) (any, error) {
		switch n := calls.Add(1); n {
		case 2:
			return nil, errors.New("boom")
		case 3:
			panic("bang")
		default:
			return sse.PatchElements(fmt.Appendf(nil, `<p id="v">%d</p>`, n)), nil
		}
	}}
	c := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the first frame", func() bool { return c.has(`<p id="v">1</p>`) })
	h.Changed([]string{"t"})
	waitFor(t, "the error", func() bool { return strings.Contains(l.String(), "boom") })
	h.Changed([]string{"t"})
	waitFor(t, "the panic", func() bool { return strings.Contains(l.String(), "bang") })
	h.Changed([]string{"t"})
	waitFor(t, "the next frame", func() bool { return c.has(`<p id="v">4</p>`) })
	if p := c.patches(); len(p) != 2 {
		t.Fatalf("got %q", p)
	}
}

// Render's context ends after RenderTimeout, 2s by default, and a render
// still running then is logged.
func TestRenderTimeout(t *testing.T) {
	deadlines := make(chan time.Duration, 1)
	h := start(t, Options{})
	serve(h, Spec{Views: []View{{Key: "v", Render: func(ctx context.Context, _ *sql.Tx) (any, error) {
		d, _ := ctx.Deadline()
		select {
		case deadlines <- time.Until(d):
		default:
		}
		return nil, nil
	}}}}, "")
	if d := <-deadlines; d <= 1900*time.Millisecond || d > 2*time.Second {
		t.Fatalf("a render has %v", d)
	}

	l := &logs{}
	h = start(t, Options{RenderTimeout: 20 * time.Millisecond, Log: l.logger()})
	serve(h, Spec{Views: []View{{Key: "slow", Render: func(ctx context.Context, _ *sql.Tx) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}}}, "")
	waitFor(t, "the overrun", func() bool { return strings.Contains(l.String(), "live: render overrun") })
	waitFor(t, "the render's error", func() bool { return strings.Contains(l.String(), context.DeadlineExceeded.Error()) })
	if !strings.Contains(l.String(), "view=slow") {
		t.Fatalf("logged %s", l.String())
	}
}

// Render's ctx comes from Run's, so it carries its values.
func TestRenderContextCarriesRunValues(t *testing.T) {
	type key struct{}
	h := New(Options{Log: slog.New(slog.DiscardHandler)})
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "run"))
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	got := make(chan any, 1)
	serve(h, Spec{Views: []View{{Key: "v", Render: func(ctx context.Context, _ *sql.Tx) (any, error) {
		select {
		case got <- ctx.Value(key{}):
		default:
		}
		return nil, nil
	}}}}, "")
	if v := <-got; v != "run" {
		t.Fatalf("Render's ctx has %v", v)
	}
}

// A view whose first render failed renders again for the next stream
// that watches it.
func TestJoinRetriesViewWithoutFrame(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	var calls atomic.Int64
	v := View{Key: "v", Render: func(context.Context, *sql.Tx) (any, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("not yet")
		}
		return sse.PatchElements([]byte(`<p id="v">ok</p>`)), nil
	}}
	serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the failed render", func() bool { return calls.Load() == 1 })
	c := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the frame", func() bool { return c.has(`<p id="v">ok</p>`) })
}

// A stream that starts watching a view during its first render gets the
// frame of that render, and the view does not render again for it. If
// that render fails, the view renders again, as for a later stream.
func TestJoinDuringFirstRender(t *testing.T) {
	for _, fails := range []bool{false, true} {
		h := start(t, Options{Tick: 2 * time.Millisecond})
		entered, release := make(chan struct{}), make(chan struct{})
		var renders atomic.Int64
		v := text("v", nil, func(context.Context, *sql.Tx) (string, error) {
			if renders.Add(1) == 1 {
				close(entered)
				<-release
				if fails {
					return "", errors.New("not yet")
				}
			}
			return "v", nil
		})
		a := serve(h, Spec{Views: []View{v}}, "")
		<-entered
		b := serve(h, Spec{Views: []View{v}}, "")
		waitFor(t, "the second stream", func() bool { return h.Count("v") == 2 })
		close(release)
		waitFor(t, "the frame", func() bool { return a.has(`<p id="v">v</p>`) && b.has(`<p id="v">v</p>`) })
		// Any render the join set off comes in a tick before the probe's.
		p := text("probe", nil, func(context.Context, *sql.Tx) (string, error) { return "p", nil })
		probe := serve(h, Spec{Views: []View{p}}, "")
		waitFor(t, "the probe", func() bool { return probe.has(`<p id="probe">p</p>`) })
		want := int64(1)
		if fails {
			want = 2
		}
		if n := renders.Load(); n != want {
			t.Fatalf("first render fails: %v; %d renders, want %d", fails, n, want)
		}
	}
}

// A render that repeats the frame before goes to no stream: compared by
// its bytes when the hub keeps the frame (two streams watch the view),
// and by its length and hash when it does not (one stream).
func TestUnchangedFrameIsNotSent(t *testing.T) {
	for _, streams := range []int{1, 2} {
		h := start(t, Options{Tick: 5 * time.Millisecond})
		v := text("v", []string{"t"}, func(context.Context, *sql.Tx) (string, error) { return "same", nil })
		var cs []*client
		for range streams {
			c := serve(h, Spec{Views: []View{v}}, "")
			waitFor(t, "the first frame", func() bool { return c.has(`<p id="v">same</p>`) })
			cs = append(cs, c)
		}
		renders := h.Stats().Renders
		for range 3 {
			h.Changed([]string{"t"})
			time.Sleep(20 * time.Millisecond)
		}
		if s := h.Stats(); s.Renders != renders+3 || s.FramesSent != int64(streams) {
			t.Fatalf("%d streams: stats %+v", streams, s)
		}
		for _, c := range cs {
			if p := c.patches(); len(p) != 1 {
				t.Fatalf("%d streams: patches %q", streams, p)
			}
		}
	}
}

// Of a view that one stream watches the hub keeps no frame: a stream that
// joins it gets one rendered in the next tick. When it repeats the frame
// before, it goes to the new stream only; when the view changed meanwhile,
// to both.
func TestJoinRendersViewWithOneStream(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	var n atomic.Int64
	v := text("v", []string{"t"}, func(context.Context, *sql.Tx) (string, error) { return strconv.Itoa(int(n.Load())), nil })
	a := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the first frame", func() bool { return a.has(`<p id="v">0</p>`) })
	ticks := h.Stats().Ticks
	b := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the second stream's frame", func() bool { return b.has(`<p id="v">0</p>`) })
	if s := h.Stats(); s.Ticks != ticks+1 || s.Renders != 2 || s.FramesSent != 2 {
		t.Fatalf("after the join: %+v", s)
	}
	b.leave(t)
	// A change that has not marked the view yet: the join's render shows it.
	n.Store(1)
	c := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the third stream's frame", func() bool { return c.has(`<p id="v">1</p>`) })
	waitFor(t, "the changed frame", func() bool { return a.has(`<p id="v">1</p>`) })
	if p := a.patches(); len(p) != 2 {
		t.Fatalf("the first stream got %q", p)
	}
}

// A stream that joins a view that two streams watch gets its newest frame
// at once, without a render.
func TestJoinGetsTheKeptFrame(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	v := text("v", []string{"t"}, func(context.Context, *sql.Tx) (string, error) { return "v", nil })
	a := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the first frame", func() bool { return a.has(`<p id="v">v</p>`) })
	b := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the second stream's frame", func() bool { return b.has(`<p id="v">v</p>`) })
	renders := h.Stats().Renders
	c := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the third stream's frame", func() bool { return c.has(`<p id="v">v</p>`) })
	if s := h.Stats(); s.Renders != renders {
		t.Fatalf("the join rendered: %+v", s)
	}
	for _, x := range []*client{a, b, c} {
		if p := x.patches(); len(p) != 1 {
			t.Fatalf("patches %q", p)
		}
	}
}

// A frame that is not []byte, on a stream without Write, is logged and
// not counted as sent.
func TestFramesSentCountsWrittenFrames(t *testing.T) {
	l := &logs{}
	h := start(t, Options{Tick: 5 * time.Millisecond, Log: l.logger()})
	odd := View{Key: "odd", Render: func(context.Context, *sql.Tx) (any, error) { return "not bytes", nil }}
	v := text("v", nil, func(context.Context, *sql.Tx) (string, error) { return "v", nil })
	c := serve(h, Spec{Views: []View{odd, v}}, "")
	waitFor(t, "a frame sent", func() bool { return h.Stats().FramesSent > 0 })
	if !c.has(`<p id="v">v</p>`) || !strings.Contains(l.String(), "not []byte") {
		t.Fatalf("got %q, logged %s", c.patches(), l.String())
	}
	if n := h.Stats().FramesSent; n != 1 {
		t.Fatalf("%d frames sent", n)
	}
}

func TestHeartbeat(t *testing.T) {
	h := start(t, Options{Heartbeat: 20 * time.Millisecond})
	v := text("v", nil, func(context.Context, *sql.Tx) (string, error) { return "v", nil })
	c := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "heartbeats", func() bool {
		n := 0
		for _, e := range c.events() {
			if e.name == "comment" {
				n++
			}
		}
		return n >= 3
	})
}

// With HeartbeatSignal, the heartbeat is a signal the page can watch, sent
// every Heartbeat while frames flow too, with a number that grows.
func TestHeartbeatSignal(t *testing.T) {
	h := start(t, Options{Tick: 2 * time.Millisecond, Heartbeat: 20 * time.Millisecond, HeartbeatSignal: "_ping"})
	var n atomic.Int64
	v := View{Key: "v", Every: 5 * time.Millisecond, Render: func(context.Context, *sql.Tx) (any, error) {
		return sse.PatchElements(fmt.Appendf(nil, `<p id="v">%d</p>`, n.Add(1))), nil
	}}
	c := serve(h, Spec{Views: []View{v}}, "")
	pings := func() []int64 {
		var out []int64
		for _, e := range c.events() {
			if e.name == "comment" {
				t.Fatal("a heartbeat signal and a comment")
			}
			if e.name != "datastar-patch-signals" {
				continue
			}
			var p struct {
				Ping int64 `json:"_ping"`
			}
			if err := json.Unmarshal([]byte(e.data), &p); err != nil {
				t.Fatalf("%q: %v", e.data, err)
			}
			out = append(out, p.Ping)
		}
		return out
	}
	waitFor(t, "three heartbeats", func() bool { return len(pings()) >= 3 })
	p := pings()
	for i := 1; i < len(p); i++ {
		if p[i] <= p[i-1] {
			t.Fatalf("the heartbeat's number does not grow: %v", p)
		}
	}
	if len(c.patches()) < 6 {
		t.Fatalf("only %d frames between the heartbeats", len(c.patches()))
	}
}

// A stream whose frames all tailor to nothing still sends heartbeats.
func TestHeartbeatWhenWriteSendsNothing(t *testing.T) {
	h := start(t, Options{Tick: 2 * time.Millisecond, Heartbeat: 30 * time.Millisecond})
	var n atomic.Int64
	v := View{Key: "v", Every: 5 * time.Millisecond, Render: func(context.Context, *sql.Tx) (any, error) {
		return n.Add(1), nil
	}}
	c := serve(h, Spec{Views: []View{v}, Write: func(*sse.Stream, string, any) error { return nil }}, "")
	waitFor(t, "heartbeats", func() bool { return len(c.events()) >= 3 })
	if n.Load() < 6 {
		t.Fatalf("only %d frames", n.Load())
	}
}

// Views that change in the same tick reach a stream together, in the
// order it watches them.
func TestViewsOfOneTickGoInOrder(t *testing.T) {
	h := start(t, Options{Tick: 2 * time.Millisecond, Workers: 4})
	var n atomic.Int64
	views := make([]View, 5)
	for i := range views {
		views[i] = text(fmt.Sprint("v", 4-i), []string{"t"}, func(context.Context, *sql.Tx) (string, error) {
			return strconv.Itoa(int(n.Load())), nil
		})
	}
	c := serve(h, Spec{Views: views}, "")
	for i := 1; i <= 20; i++ {
		n.Store(int64(i))
		h.Changed([]string{"t"})
		waitFor(t, "the tick's frames", func() bool { return c.has(fmt.Sprintf(`<p id="v0">%d</p>`, i)) })
	}
	p := c.patches()
	for i := 0; i+5 <= len(p); i += 5 {
		for j := range 5 {
			if !strings.HasPrefix(p[i+j], fmt.Sprintf(`<p id="v%d">`, 4-j)) {
				t.Fatalf("out of order: %q", p[i:i+5])
			}
		}
	}
}

// blockingWrite records the frames a stream writes as key and value, and
// blocks on the frame first until release is closed.
type blockingWrite struct {
	first            string
	entered, release chan struct{}

	mu  sync.Mutex
	got []string
}

func newBlockingWrite(first string) *blockingWrite {
	return &blockingWrite{first: first, entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingWrite) write(_ *sse.Stream, key string, frame any) error {
	if p, ok := frame.([]byte); ok {
		frame = string(p)
	}
	f := fmt.Sprint(key, frame)
	b.mu.Lock()
	b.got = append(b.got, f)
	b.mu.Unlock()
	if f == b.first {
		close(b.entered)
		<-b.release
	}
	return nil
}

func (b *blockingWrite) sent() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.got)
}

// numbers returns views that render what n holds, on topic "t".
func numbers(n *atomic.Int64, keys ...string) []View {
	vs := make([]View, len(keys))
	for i, k := range keys {
		vs[i] = View{Key: k, Topics: []string{"t"}, Render: func(context.Context, *sql.Tx) (any, error) { return n.Load(), nil }}
	}
	return vs
}

// byteNumbers is numbers with []byte frames, which the hub does not keep
// for a view that one stream watches.
func byteNumbers(n *atomic.Int64, keys ...string) []View {
	vs := numbers(n, keys...)
	for i := range vs {
		vs[i].Render = func(context.Context, *sql.Tx) (any, error) { return strconv.AppendInt(nil, n.Load(), 10), nil }
	}
	return vs
}

// While a stream's Write blocks, the frames of later ticks wait: each
// tick's frames go out together and in the stream's order, and a frame
// that a newer one replaced before it went out counts as dropped. So it
// goes with []byte frames too, of which the hub keeps none: a stream's
// pending frames are its own.
func TestBlockedWriteKeepsTicksTogether(t *testing.T) {
	for name, views := range map[string]func(*atomic.Int64, ...string) []View{"int": numbers, "[]byte": byteNumbers} {
		h := start(t, Options{Tick: 2 * time.Millisecond})
		var n atomic.Int64
		n.Store(1)
		b := newBlockingWrite("a1")
		serve(h, Spec{Views: views(&n, "a", "b"), Write: b.write}, "")
		<-b.entered
		for i := int64(2); i <= 3; i++ {
			n.Store(i)
			h.Changed([]string{"t"})
			waitFor(t, "the tick", func() bool { return h.Stats().Ticks == i })
		}
		waitFor(t, "the third tick's frames", func() bool { return h.Stats().FramesDropped == 2 })
		close(b.release)
		waitFor(t, "the last frames", func() bool {
			got := b.sent()
			return slices.Contains(got, "a3") && slices.Contains(got, "b3")
		})
		if got, want := b.sent(), []string{"a1", "b1", "a3", "b3"}; !slices.Equal(got, want) {
			t.Fatalf("%s: sent %v, want %v", name, got, want)
		}
		waitFor(t, "the count", func() bool { return h.Stats().FramesSent == 4 })
		if s := h.Stats(); s.FramesDropped != 2 {
			t.Fatalf("%s: stats %+v", name, s)
		}
	}
}

// Once Unwatch returns, the stream sends no frame of the view that it has
// not started to write, not even one it took along with a frame it is
// still writing.
func TestNoFrameAfterUnwatch(t *testing.T) {
	h := start(t, Options{Tick: 2 * time.Millisecond})
	var n atomic.Int64
	n.Store(1)
	b := newBlockingWrite("a1")
	serve(h, Spec{ID: "tab", Views: numbers(&n, "a", "b"), Write: b.write}, "")
	<-b.entered
	if !h.Unwatch("tab", "b") {
		t.Fatal("Unwatch")
	}
	close(b.release)
	n.Store(2)
	h.Changed([]string{"t"})
	waitFor(t, "the next frame", func() bool { return slices.Contains(b.sent(), "a2") })
	if got, want := b.sent(), []string{"a1", "a2"}; !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
}

// With Write, the stream tailors each frame: here it sends only the rows
// of a list that it has not sent yet.
func TestWriteTailorsFrames(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	var mu sync.Mutex
	rows := []string{"a", "b", "c"}
	list := View{Key: "list", Topics: []string{"list"}, Render: func(context.Context, *sql.Tx) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(rows), nil
	}}
	tailored := func() func(*sse.Stream, string, any) error {
		var had []string
		return func(s *sse.Stream, key string, frame any) error {
			if key != "list" {
				return fmt.Errorf("key %q", key)
			}
			now := frame.([]string)
			var events [][]byte
			for i, r := range now {
				if i >= len(had) || had[i] != r {
					events = append(events, sse.PatchElements(fmt.Appendf(nil, `<li id="r%d">%s</li>`, i, r)))
				}
			}
			had = now
			return s.Write(events...)
		}
	}
	a := serve(h, Spec{Views: []View{list}, Write: tailored()}, "")
	waitFor(t, "the list", func() bool { return len(a.patches()) == 3 })
	mu.Lock()
	rows = []string{"a", "B", "c"}
	mu.Unlock()
	h.Changed([]string{"list"})
	waitFor(t, "the changed row", func() bool { return a.has(`<li id="r1">B</li>`) })
	if p := a.patches(); len(p) != 4 {
		t.Fatalf("got %q", p)
	}
	b := serve(h, Spec{Views: []View{list}, Write: tailored()}, "")
	waitFor(t, "the whole list for a new stream", func() bool { return len(b.patches()) == 3 && b.has(`<li id="r1">B</li>`) })
}

// countingClient counts the writes that reach the client.
type countingClient struct {
	*client
	writes atomic.Int64
}

func (c *countingClient) Write(p []byte) (int, error) {
	c.writes.Add(1)
	return c.client.Write(p)
}

// WriteTick gets the frames of a tick in one call, in the order the stream
// watches the views, so they reach the client in one compressed write,
// where Write makes a write of each.
func TestWriteTick(t *testing.T) {
	h := start(t, Options{Tick: 2 * time.Millisecond})
	var n atomic.Int64
	n.Store(1)
	views := numbers(&n, "b", "a")
	event := func(key string, frame any) []byte {
		return sse.PatchElements(fmt.Appendf(nil, `<p id="%s">%d</p>`, key, frame))
	}
	var mu sync.Mutex
	var calls [][]string
	tick := func(s *sse.Stream, frames []Frame) error {
		var got []string
		var events [][]byte
		for _, f := range frames {
			got = append(got, fmt.Sprint(f.Key, f.Value))
			events = append(events, event(f.Key, f.Value))
		}
		mu.Lock()
		calls = append(calls, got)
		mu.Unlock()
		return s.Write(events...)
	}
	each := func(s *sse.Stream, key string, frame any) error { return s.Write(event(key, frame)) }
	open := func(spec Spec) *countingClient {
		var cc *countingClient
		serveThrough(h, spec, sse.Zstd, func(c *client) http.ResponseWriter {
			cc = &countingClient{client: c}
			return cc
		})
		return cc
	}
	byTick := open(Spec{Views: views, WriteTick: tick, Write: func(*sse.Stream, string, any) error {
		return errors.New("Write was called beside WriteTick")
	}})
	byView := open(Spec{Views: views, Write: each})
	has := func(c *countingClient, n int64) bool {
		b := unzstd(t, c.bytes())
		return bytes.Contains(b, fmt.Appendf(nil, `<p id="a">%d</p>`, n)) && bytes.Contains(b, fmt.Appendf(nil, `<p id="b">%d</p>`, n))
	}
	waitFor(t, "the first frames", func() bool { return has(byTick, 1) && has(byView, 1) })
	tickWrites, viewWrites := byTick.writes.Load(), byView.writes.Load()
	n.Store(2)
	h.Changed([]string{"t"})
	waitFor(t, "the second frames", func() bool { return has(byTick, 2) && has(byView, 2) })
	if d := byTick.writes.Load() - tickWrites; d != 1 {
		t.Errorf("WriteTick: %d writes for a tick of two frames", d)
	}
	if d := byView.writes.Load() - viewWrites; d != 2 {
		t.Errorf("Write: %d writes for a tick of two frames", d)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := [][]string{{"b1", "a1"}, {"b2", "a2"}}; !slices.EqualFunc(calls, want, slices.Equal) {
		t.Fatalf("WriteTick got %q, want %q", calls, want)
	}
	if s := h.Stats(); s.FramesSent != 8 {
		t.Fatalf("%d frames sent, want 4 to each stream", s.FramesSent)
	}
}

// A new stream with the ID of an open one replaces it.
func TestSameIDReplacesStream(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	v := text("v", nil, func(context.Context, *sql.Tx) (string, error) { return "v", nil })
	w := text("w", nil, func(context.Context, *sql.Tx) (string, error) { return "w", nil })
	old := serve(h, Spec{ID: "tab", Views: []View{v}}, "")
	waitFor(t, "the old stream", func() bool { return old.has(`<p id="v">v</p>`) })
	cur := serve(h, Spec{ID: "tab", Views: []View{v}}, "")
	select {
	case err := <-old.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the old stream stayed open")
	}
	waitFor(t, "the new stream", func() bool { return cur.has(`<p id="v">v</p>`) })
	if !h.Watch("tab", w) {
		t.Fatal("Watch")
	}
	waitFor(t, "the watched view", func() bool { return cur.has(`<p id="w">w</p>`) })
	if h.Count("v") != 1 {
		t.Fatalf("%d streams on v", h.Count("v"))
	}
}

// slowOpen holds the stream's first flush, which sends its headers, until
// release is closed.
type slowOpen struct {
	*client
	entered, release chan struct{}
	once             sync.Once
}

func (c *slowOpen) FlushError() error {
	c.once.Do(func() {
		close(c.entered)
		<-c.release
	})
	return nil
}

// Of two streams with the same ID, the later request stays, even when
// the earlier one takes longer to open.
func TestSameIDKeepsLaterStream(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	v := text("v", nil, func(context.Context, *sql.Tx) (string, error) { return "v", nil })
	w := text("w", nil, func(context.Context, *sql.Tx) (string, error) { return "w", nil })
	slow := &slowOpen{entered: make(chan struct{}), release: make(chan struct{})}
	earlier := serveThrough(h, Spec{ID: "tab", Views: []View{v}}, "", func(c *client) http.ResponseWriter {
		slow.client = c
		return slow
	})
	<-slow.entered
	later := serve(h, Spec{ID: "tab", Views: []View{v}}, "")
	waitFor(t, "the later stream", func() bool { return later.has(`<p id="v">v</p>`) })
	close(slow.release)
	select {
	case err := <-earlier.done:
		if err != nil {
			t.Fatal(err)
		}
	case err := <-later.done:
		t.Fatalf("the earlier stream replaced the later one, which returned %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("both streams stayed open")
	}
	if !h.Watch("tab", w) {
		t.Fatal("Watch")
	}
	waitFor(t, "the watched view", func() bool { return later.has(`<p id="w">w</p>`) })
	if n := h.Count("v"); n != 1 {
		t.Fatalf("%d streams on v", n)
	}
}

// After a tick runs over, the hub samples tick durations for a while and
// then logs their distribution.
func TestOverrunReport(t *testing.T) {
	l := &logs{}
	h := New(Options{Tick: 5 * time.Millisecond, Log: l.logger()})
	h.sampleFor = 100 * time.Millisecond
	ctx := t.Context()
	go h.Run(ctx)
	v := View{Key: "slow", Topics: []string{"t"}, Render: func(context.Context, *sql.Tx) (any, error) {
		time.Sleep(15 * time.Millisecond)
		return nil, nil
	}}
	serve(h, Spec{Views: []View{v}}, "")
	for range 5 {
		h.Changed([]string{"t"})
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(t, "the report", func() bool { return strings.Contains(l.String(), "tick overrun") })
	for _, k := range []string{"n=", "p50=", "p95=", "p99=", "max="} {
		if !strings.Contains(l.String(), k) {
			t.Fatalf("no %s in %s", k, l.String())
		}
	}
	if h.Stats().Overruns == 0 {
		t.Fatal("no overruns counted")
	}
}

// The report of an overrun does not outlive Run.
func TestNoOverrunReportAfterRun(t *testing.T) {
	l := &logs{}
	h := New(Options{Tick: 5 * time.Millisecond, Log: l.logger()})
	h.sampleFor = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- h.Run(ctx) }()
	v := View{Key: "slow", Render: func(context.Context, *sql.Tx) (any, error) {
		time.Sleep(15 * time.Millisecond)
		return nil, nil
	}}
	serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "an overrun", func() bool { return h.Stats().Overruns > 0 })
	cancel()
	if err := <-ran; err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * h.sampleFor)
	if strings.Contains(l.String(), "tick overrun") {
		t.Fatal("reported an overrun after Run returned")
	}
}

// OnTick gets every tick that rendered views, and its whole duration.
func TestOnTick(t *testing.T) {
	var mu sync.Mutex
	var took []time.Duration
	ticks := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(took)
	}
	h := start(t, Options{Tick: 5 * time.Millisecond, OnTick: func(d time.Duration) {
		mu.Lock()
		took = append(took, d)
		mu.Unlock()
	}})
	var n atomic.Int64
	v := text("slow", []string{"t"}, func(context.Context, *sql.Tx) (string, error) {
		time.Sleep(3 * time.Millisecond)
		return strconv.Itoa(int(n.Load())), nil
	})
	c := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the first frame", func() bool { return c.has(`<p id="slow">0</p>`) })
	for i := 1; i <= 3; i++ {
		n.Store(int64(i))
		h.Changed([]string{"t"})
		waitFor(t, "a tick", func() bool { return ticks() == i+1 })
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if s := h.Stats(); s.Ticks != 4 || len(took) != 4 {
		t.Fatalf("OnTick saw %d ticks, the hub counted %d", len(took), s.Ticks)
	}
	for _, d := range took {
		if d < 3*time.Millisecond {
			t.Fatalf("a tick that rendered for 3ms took %v", d)
		}
	}
}

// Done stays open while the hub runs, and closes when Run's ctx ends.
func TestDone(t *testing.T) {
	h := New(Options{Log: slog.New(slog.DiscardHandler)})
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- h.Run(ctx) }()
	select {
	case <-h.Done():
		t.Fatal("Done closed while the hub runs")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done stayed open after the hub stopped")
	}
	if err := <-ran; err != nil {
		t.Fatal(err)
	}
}

func TestRunTwice(t *testing.T) {
	h := start(t, Options{})
	time.Sleep(10 * time.Millisecond)
	if err := h.Run(context.Background()); err == nil {
		t.Fatal("ran twice")
	}
}
