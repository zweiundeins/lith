package live

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// inUse waits for the pool to hold n connections in use.
func inUse(t *testing.T, stats func() int, n int, what string) {
	t.Helper()
	waitFor(t, what, func() bool { return stats() == n })
}

// A hub at rest gives its workers' connections back, its next change renders
// on connections taken again, with statements prepared again, on a pool of
// one or two, and with two workers on a pool of three (a smaller pool caps
// them at one).
func TestHubReleasesItsConnections(t *testing.T) {
	for _, c := range []struct {
		name          string
		pool, workers int
	}{
		{"pool of 1", 1, 1},
		{"pool of 2", 2, 1},
		{"pool of 3, two workers", 3, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			var lg logs
			write, read := testDB(t, c.pool)
			h := start(t, Options{Read: read, Workers: c.workers, Tick: 2 * time.Millisecond, ReleaseAfter: 30 * time.Millisecond, Log: lg.logger()})
			used := func() int { return read.Stats().InUse }
			var renders atomic.Int64
			keys := []string{"n"}
			if c.workers > 1 {
				keys = append(keys, "m") // one view per worker, so each gets the tick
			}
			var views []View
			for _, k := range keys {
				views = append(views, counterView(k, &renders))
			}
			cl := serve(h, Spec{Views: views}, "")
			frames := func(i int) func() bool {
				return func() bool {
					return !slices.ContainsFunc(keys, func(k string) bool { return !cl.has(fmt.Sprintf(`<p id="%s">%d</p>`, k, i)) })
				}
			}
			waitFor(t, "the first frames", frames(0))
			inUse(t, used, 0, "the connections given back")
			if c.workers > 1 && strings.Contains(lg.String(), "too few connections") {
				t.Fatalf("the pool capped the workers: %s", lg.String())
			}

			for i := 1; i <= 5; i++ {
				increment(t, write)
				h.Changed([]string{"counter"})
				waitFor(t, "the next frames", frames(i))
				// While the hub renders, GET-like reads still find the pool.
				var n int
				if err := read.QueryRow(`select n from counter where name = 'a'`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				inUse(t, used, 0, "the connections given back after the change")
			}
			if st := h.Stats(); st.Ticks < 6 || st.ReadTxs < 6 {
				t.Fatalf("stats: %+v", st)
			}
		})
	}
}

// The workers keep their connections while the hub is busy, with the same
// prepared statements, and take none again until there is work.
func TestHubKeepsConnectionsWhileBusy(t *testing.T) {
	write, read, prepares := countingDB(t, 2)
	h := start(t, Options{Read: read, Workers: 1, Tick: 2 * time.Millisecond, ReleaseAfter: 30 * time.Millisecond})
	var renders atomic.Int64
	cl := serve(h, Spec{Views: []View{counterView("n", &renders)}}, "")
	waitFor(t, "the first frame", func() bool { return cl.has(`<p id="n">0</p>`) })

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := write.Exec(`update counter set n = n + 1`); err != nil {
				t.Error(err)
				return
			}
			h.Changed([]string{"counter"})
			time.Sleep(3 * time.Millisecond)
		}
	}()
	time.Sleep(100 * time.Millisecond)
	if st := read.Stats(); st.InUse != 1 {
		t.Errorf("while busy: %+v", st)
	}
	before := prepares.Load()
	time.Sleep(100 * time.Millisecond)
	close(stop)
	<-done
	if d := prepares.Load() - before; d != 0 {
		t.Errorf("%d statements prepared again while the hub was busy", d)
	}
}

// With the pool's only connection taken, a tick waits up to RenderTimeout
// for one and leaves its views to the next tick. That logs one warning for
// the whole wait, and neither an error nor a tick or an overrun; the views
// render once the pool has a connection to give.
func TestHubAcquireWaitsAndRetries(t *testing.T) {
	var lg logs
	var onTicks atomic.Int64
	write, read := testDB(t, 1)
	h := start(t, Options{Read: read, Workers: 1, Tick: 2 * time.Millisecond, ReleaseAfter: 20 * time.Millisecond, RenderTimeout: 20 * time.Millisecond, Log: lg.logger(),
		OnTick: func(time.Duration) { onTicks.Add(1) }})
	var renders atomic.Int64
	cl := serve(h, Spec{Views: []View{counterView("n", &renders)}}, "")
	waitFor(t, "the first frame", func() bool { return cl.has(`<p id="n">0</p>`) })
	waitFor(t, "the connection given back", func() bool { return read.Stats().InUse == 0 })

	give := takeConns(t, read, 1)
	before, onBefore := h.Stats(), onTicks.Load()
	increment(t, write)
	h.Changed([]string{"counter"})
	waitFor(t, "the warning", func() bool { return strings.Contains(lg.String(), "no read connection") })
	time.Sleep(100 * time.Millisecond) // five RenderTimeouts more
	const one = `<p id="n">1</p>`
	if cl.has(one) {
		t.Fatal("rendered without a connection")
	}
	if st := h.Stats(); st.Ticks != before.Ticks || st.Overruns != before.Overruns {
		t.Errorf("a tick without a connection counted: ticks %d to %d, overruns %d to %d", before.Ticks, st.Ticks, before.Overruns, st.Overruns)
	}
	if n := onTicks.Load(); n != onBefore {
		t.Errorf("OnTick ran %d times without a render", n-onBefore)
	}
	logged := lg.String()
	if n := strings.Count(logged, "no read connection"); n != 1 {
		t.Errorf("%d warnings while the pool was taken, want one: %s", n, logged)
	}
	if strings.Contains(logged, "level=ERROR") {
		t.Errorf("an error logged for a busy pool: %s", logged)
	}
	give()
	waitFor(t, "the render once the pool has a connection", func() bool { return cl.has(one) })
}

// With two workers and one connection free after a release, the worker that
// gets it renders the tick, and the other one stops waiting for a connection
// that the tick doesn't need.
func TestHubTakesOnlyTheConnectionsItRendersWith(t *testing.T) {
	var lg logs
	write, read := testDB(t, 4)
	h := start(t, Options{Read: read, Workers: 2, Tick: 2 * time.Millisecond, ReleaseAfter: 30 * time.Millisecond, RenderTimeout: 3 * time.Second, Log: lg.logger()})
	cl := serve(h, Spec{Views: []View{counterText("a"), counterText("b")}}, "")
	frames := func(row string) func() bool {
		return func() bool { return cl.has(`<p id="a">`+row+`</p>`) && cl.has(`<p id="b">`+row+`</p>`) }
	}
	waitFor(t, "the first frames", frames("row 0"))
	waitFor(t, "the connections given back", func() bool { return read.Stats().InUse == 0 })

	give := takeConns(t, read, 3) // GET-like reads hold three of the four
	defer give()
	increment(t, write)
	began := time.Now()
	h.Changed([]string{"counter"})
	waitFor(t, "the next frames", frames("row 1"))
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("the frames took %v with a connection free for the tick (RenderTimeout 3s)", took)
	}
	if logged := lg.String(); strings.Contains(logged, "level=ERROR") || strings.Contains(logged, "no read connection") {
		t.Errorf("the worker without a connection was reported: %s", logged)
	}
}

// A stream that opens on a released hub while the pool is busy waits for the
// connection, past RenderTimeout, and opens with its first frame.
func TestServeWaitsForAReadConnection(t *testing.T) {
	_, read := testDB(t, 1)
	const timeout = 20 * time.Millisecond
	h := start(t, Options{Read: read, Workers: 1, Tick: 2 * time.Millisecond, ReleaseAfter: 20 * time.Millisecond, RenderTimeout: timeout})
	first := serve(h, Spec{Views: []View{counterText("n")}}, "")
	waitFor(t, "the first frame", func() bool { return first.has(`<p id="n">row 0</p>`) })
	waitFor(t, "the connection given back", func() bool { return read.Stats().InUse == 0 })

	give := takeConns(t, read, 1)
	defer give()
	var fc *flushClock
	cl := serveThrough(h, Spec{Views: []View{counterText("m")}}, "", func(c *client) http.ResponseWriter {
		fc = &flushClock{client: c}
		return fc
	})
	time.Sleep(5 * timeout)
	if fc.at.Load() != 0 {
		t.Fatal("the headers went out before the connection came back")
	}
	freed := time.Now()
	give()
	waitFor(t, "the response", func() bool { return fc.at.Load() != 0 })
	if at := time.Unix(0, fc.at.Load()); at.Before(freed) {
		t.Fatalf("the headers went out %v before the connection came back", freed.Sub(at))
	}
	waitFor(t, "the first frame", func() bool { return cl.has(`<p id="m">row 0</p>`) })
}

// Without ReleaseAfter, a worker whose connection went bad waits for a new
// one as long as it takes: RenderTimeout bounds a Render, not that wait, and
// the tick renders without logging anything.
func TestWorkerWaitsForAFreshConnection(t *testing.T) {
	var lg logs
	db := newFakeDB(t, 2, 1, 0)
	h := start(t, Options{Read: db.DB, Workers: 1, Tick: 2 * time.Millisecond, RenderTimeout: 20 * time.Millisecond, Log: lg.logger()})
	waitFor(t, "the worker's connection", func() bool { return db.opened.Load() == 1 })
	gate := make(chan struct{})
	db.gate.Store(&gate) // the pool opens no connection until the gate opens
	cl := serve(h, Spec{Views: slowViews(1)}, "")
	time.Sleep(100 * time.Millisecond) // five RenderTimeouts
	const frame = `<p id="v0">v0</p>`
	if cl.has(frame) {
		t.Fatal("rendered without a connection")
	}
	if logged := lg.String(); strings.Contains(logged, "level=ERROR") || strings.Contains(logged, "no read connection") {
		t.Errorf("the wait for a connection was reported: %s", logged)
	}
	close(gate)
	waitFor(t, "the view", func() bool { return cl.has(frame) })
	if n := h.Stats().Ticks; n != 1 {
		t.Errorf("the view took %d ticks", n)
	}
}

// counterText is a view of the counter under key, rendered as "row N".
func counterText(key string) View {
	return text(key, []string{"counter"}, func(ctx context.Context, tx *sql.Tx) (string, error) {
		n, err := count(ctx, tx)
		return "row " + strconv.Itoa(n), err
	})
}

// takeConns takes n connections of the pool and returns a func that gives
// them back; it may be called twice.
func takeConns(t *testing.T, read *sql.DB, n int) func() {
	t.Helper()
	var cs []*sql.Conn
	for range n {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := read.Conn(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	return sync.OnceFunc(func() {
		for _, c := range cs {
			c.Close()
		}
	})
}

// flushClock records when Serve first flushed the response, which sse.Open
// does once it has sent the headers.
type flushClock struct {
	*client
	at atomic.Int64
}

func (f *flushClock) FlushError() error {
	f.at.CompareAndSwap(0, time.Now().UnixNano())
	return nil
}
