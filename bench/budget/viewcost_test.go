package budget

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"
)

// tableView is a tab's view of a data table of rows rows, rendered as an
// app does: into a buffer, then formatted as an event. version changes a
// few cells.
func tableView(key string, rows int, version *atomic.Int64) live.View {
	return live.View{
		Key:    key,
		Topics: []string{"table"},
		Render: func(context.Context, *sql.Tx) (any, error) {
			v := int(version.Load())
			var b bytes.Buffer
			b.WriteString(`<sb-data-table id="stars" name="stars" sort="mag"><table role="grid"><tbody>` + "\n")
			for i := range rows {
				mag := float64((i*7919+v*(i%5))%1300)/100 - 1
				fmt.Fprintf(&b, `<tr id="star-%d" aria-rowindex="%d" data-key="%d"><td class="name"><a href="/stars/%d">HD %d</a></td><td><span class="cls cls--G">G2V</span></td><td>Cassiopeia</td><td class="num">%.2f</td><td class="num">%.4f</td><td class="num">%+.4f</td><td class="num">%.1f mas/yr</td><td class="num">%d ly</td></tr>`+"\n",
					i, i+2, i, i, 1000+i*7, mag, float64(i%360)+0.1234, float64(i%180)-90.5678, float64(i%400)/3, 4+i%4000)
			}
			b.WriteString(`</tbody></table></sb-data-table>`)
			return sse.PatchElements(b.Bytes()), nil
		},
	}
}

// countingSink is a client that takes everything and counts its flushes.
type countingSink struct {
	header  http.Header
	flushes atomic.Int64
}

func (s *countingSink) Header() http.Header         { return s.header }
func (s *countingSink) WriteHeader(int)             {}
func (s *countingSink) Write(p []byte) (int, error) { return len(p), nil }
func (s *countingSink) Flush()                      { s.flushes.Add(1) }

// viewCost is what a hub's streams held after a session.
type viewCost struct {
	frame       int     // bytes of the view's frame
	heap, stack float64 // bytes per stream, live after a collection
	alloc       float64 // bytes allocated per stream and frame: renders and writes
}

// measureViews opens streams streams on a hub, each watching a table view
// of rows rows (its own, one for every per streams, or with per 0 one they
// all share), writes frames frames to every stream and measures what
// stays on the heap and the stacks per stream.
func measureViews(t *testing.T, rows, streams, frames, per int, coding string, opt sse.Options) viewCost {
	t.Helper()
	before, stack0 := liveHeap(), stackInUse()
	ctx, cancel := context.WithCancel(t.Context())
	opt.Budget = sse.NewBudget(sse.BudgetOptions{Limit: -1})
	h := live.New(live.Options{SSE: opt, Tick: 5 * time.Millisecond, Heartbeat: time.Hour})
	ran := make(chan struct{})
	go func() { h.Run(ctx); close(ran) }()
	var version atomic.Int64
	shared := tableView("table", rows, &version)
	sinks := make([]*countingSink, streams)
	var wg sync.WaitGroup
	for i := range sinks {
		sinks[i] = &countingSink{header: http.Header{}}
		v := shared
		if per > 0 {
			v = tableView("table:"+strconv.Itoa(i/per), rows, &version)
		}
		r := httptest.NewRequestWithContext(ctx, "GET", "/", nil)
		r.Header.Set("Accept-Encoding", cmpOr(coding, "identity"))
		wg.Go(func() { h.Serve(sinks[i], r, live.Spec{Views: []live.View{v}}) })
	}
	// Each stream flushes its headers, then one write per frame.
	wait := func(n int64) {
		for deadline := time.Now().Add(time.Minute); ; {
			done := true
			for _, s := range sinks {
				if s.flushes.Load() < n {
					done = false
					break
				}
			}
			if done {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the streams did not get %d writes in a minute", n)
			}
			time.Sleep(time.Millisecond)
		}
	}
	wait(2)
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	for k := 1; k < frames; k++ {
		version.Add(1)
		h.Changed([]string{"table"})
		wait(int64(k) + 2)
	}
	runtime.ReadMemStats(&m1)
	frame := 0
	if f, err := shared.Render(ctx, nil); err == nil {
		frame = len(f.([]byte))
	}
	c := viewCost{
		frame: frame,
		heap:  float64(liveHeap()-before) / float64(streams),
		stack: float64(stackInUse()-stack0) / float64(streams),
		alloc: float64(m1.TotalAlloc-m0.TotalAlloc) / float64(streams*(frames-1)),
	}
	cancel()
	wg.Wait()
	<-ran
	return c
}

func stackInUse() int64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.StackInuse)
}

// measureConns opens n connections to a server that holds them open, and
// measures what they hold on the heap and the stacks per connection, the
// client's side included.
func measureConns(t *testing.T, n int, h http.Handler) (heap, stack float64) {
	t.Helper()
	var srv *httptest.Server
	var accepted []net.Conn
	var mu sync.Mutex
	addr := ""
	if h != nil {
		srv = httptest.NewServer(h)
		addr = srv.Listener.Addr().String()
	} else {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		addr = ln.Addr().String()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				mu.Lock()
				accepted = append(accepted, c)
				mu.Unlock()
			}
		}()
	}
	before, stack0 := liveHeap(), stackInUse()
	conns := make([]net.Conn, n)
	for i := range conns {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		if h != nil {
			io.WriteString(c, "GET / HTTP/1.1\r\nHost: lith\r\n\r\n")
			r := bufio.NewReaderSize(c, 16)
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				if l == "\r\n" {
					break
				}
			}
		}
		go io.Copy(io.Discard, c)
		conns[i] = c
	}
	time.Sleep(100 * time.Millisecond)
	heap, stack = float64(liveHeap()-before)/float64(n), float64(stackInUse()-stack0)/float64(n)
	for _, c := range conns {
		c.Close()
	}
	mu.Lock()
	for _, c := range accepted {
		c.Close()
	}
	mu.Unlock()
	if srv != nil {
		srv.Close()
	}
	return heap, stack
}

// TestViewCost measures what a live hub holds per stream besides the
// encoder: 64 streams that each watch a view of their own (a tab's view),
// one view for every two streams (whose frame the hub keeps) or one view
// they all share, with data tables of about 50, 142 and 230 KB (and of 0.8
// KB, for what a view holds besides its frame), uncompressed and with the
// encoders the budget hands out. It also measures what net/http holds for
// an open HTTP/1.1 connection: connections to a handler that holds them
// open, less the same connections to a listener that accepts them and
// does nothing. It writes viewcost.csv.
func TestViewCost(t *testing.T) {
	request(t)
	const streams, frames = 64, 20
	f, err := os.Create("viewcost.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := csv.NewWriter(f)
	c.Write([]string{"rows", "frame_bytes", "setting", "views", "streams", "heap_kib_per_stream", "stack_kib_per_stream", "alloc_kib_per_stream_frame"})
	const conns = 200
	rawHeap, rawStack := measureConns(t, conns, nil)
	httpHeap, httpStack := measureConns(t, conns, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	c.Write([]string{"0", "0", "net/http connection", "none", strconv.Itoa(conns),
		strconv.FormatFloat((httpHeap-rawHeap)/1024, 'f', 1, 64), strconv.FormatFloat((httpStack-rawStack)/1024, 'f', 1, 64), "0"})
	t.Logf("net/http: %.1f KiB heap, %.1f KiB stack per connection", (httpHeap-rawHeap)/1024, (httpStack-rawStack)/1024)
	for _, rows := range []int{2, 150, 430, 700} {
		for _, s := range []setting{{}, {coding: sse.Brotli, window: 18, q: 3}, {coding: sse.Brotli, window: 18, q: 4}, {coding: sse.Zstd, window: 18, lowMem: true}} {
			for _, per := range []int{1, 2, 0} {
				views := map[int]string{1: "own", 2: "pair", 0: "shared"}[per]
				r := measureViews(t, rows, streams, frames, per, s.coding, s.options())
				c.Write([]string{strconv.Itoa(rows), strconv.Itoa(r.frame), s.String(), views, strconv.Itoa(streams),
					strconv.FormatFloat(r.heap/1024, 'f', 0, 64), strconv.FormatFloat(r.stack/1024, 'f', 1, 64), strconv.FormatFloat(r.alloc/1024, 'f', 0, 64)})
				c.Flush()
				t.Logf("%4d rows (%6d B) %-22s %-6s %6.0f KiB heap  %5.1f KiB stack per stream  %5.0f KiB allocated per stream and frame",
					rows, r.frame, s, views, r.heap/1024, r.stack/1024, r.alloc/1024)
			}
		}
	}
	if err := c.Error(); err != nil {
		t.Fatal(err)
	}
}
