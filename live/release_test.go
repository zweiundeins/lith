package live

import (
	"context"
	"fmt"
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
// one or two.
func TestHubReleasesItsConnections(t *testing.T) {
	for _, c := range []struct {
		name          string
		pool, workers int
	}{
		{"pool of 1", 1, 1},
		{"pool of 2", 2, 1},
		{"pool of 2, two workers", 2, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			write, read := testDB(t, c.pool)
			h := start(t, Options{Read: read, Workers: c.workers, Tick: 2 * time.Millisecond, ReleaseAfter: 30 * time.Millisecond})
			used := func() int { return read.Stats().InUse }
			var renders atomic.Int64
			cl := serve(h, Spec{Views: []View{counterView("n", &renders)}}, "")
			waitFor(t, "the first frame", func() bool { return cl.has(`<p id="n">0</p>`) })
			inUse(t, used, 0, "the connections given back")

			for i := 1; i <= 5; i++ {
				increment(t, write)
				h.Changed([]string{"counter"})
				want := fmt.Sprintf(`<p id="n">%d</p>`, i)
				waitFor(t, "the next frame", func() bool { return cl.has(want) })
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

// With the pool's only connection taken, a tick that needs one waits up to
// RenderTimeout and comes again; it renders once the pool has one to give.
func TestHubAcquireWaitsAndRetries(t *testing.T) {
	write, read := testDB(t, 1)
	h := start(t, Options{Read: read, Workers: 1, Tick: 2 * time.Millisecond, ReleaseAfter: 20 * time.Millisecond, RenderTimeout: 20 * time.Millisecond})
	var renders atomic.Int64
	cl := serve(h, Spec{Views: []View{counterView("n", &renders)}}, "")
	waitFor(t, "the first frame", func() bool { return cl.has(`<p id="n">0</p>`) })
	waitFor(t, "the connection given back", func() bool { return read.Stats().InUse == 0 })

	ctx := context.Background()
	c, err := read.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	increment(t, write)
	h.Changed([]string{"counter"})
	time.Sleep(80 * time.Millisecond)
	const one = `<p id="n">1</p>`
	if cl.has(one) {
		t.Fatal("rendered without a connection")
	}
	c.Close()
	waitFor(t, "the render once the pool has a connection", func() bool { return cl.has(one) })
}
