package budget

import (
	"context"
	"encoding/csv"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"
)

// curveRow is one budget the curve opens streams against.
type curveRow struct {
	rows    int    // the table's rows: 150, 430 or 700 for 50, 142 or 230 KB
	mib     int64  // the budget's limit
	mix     string // "br", "zstd" or "mixed" (alternating, zstd first)
	streams int
	own     bool // each stream watches a view of its own, else all share one
}

// curveResult is what the budget held once every stream had opened.
type curveResult struct {
	frame                  int
	opened, held, coded    int // streams opened, at a window that holds their frame, compressed
	firstBare              int // the first stream whose first write went out uncompressed or in raw blocks, 0 for none
	reserved, frames, heap int64
	perFrame               float64 // bytes per frame on the wire, the last round, over every stream
	tiers                  map[string]int
}

var tierWindow = regexp.MustCompile(`2\^(\d+)`)

// holdsFrame reports whether a tier, as BudgetStats names it, holds a
// frame of n bytes in its window with a sixteenth to spare, as the budget
// counts it.
func holdsFrame(tier string, n int) bool {
	m := tierWindow.FindStringSubmatch(tier)
	if m == nil {
		return false
	}
	w, _ := strconv.Atoi(m[1])
	return int64(n)*16 <= int64(15)<<w
}

// curve opens the row's streams one by one on a hub with a budget, each
// with a tab's table view of its own or a view they share. A stream's
// first write picks its tier; after every ten opens, and after the last,
// every view changes and every stream writes, so that zstd streams move
// and waiting streams take the room that frees up. It returns what the
// budget books and what the heap and the stacks hold after a collection.
func curve(t *testing.T, r curveRow) curveResult {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b := sse.NewBudget(sse.BudgetOptions{Limit: r.mib << 20})
	h := live.New(live.Options{SSE: sse.Options{Budget: b}, Tick: 2 * time.Millisecond, Heartbeat: time.Hour})
	ran := make(chan struct{})
	go func() { h.Run(ctx); close(ran) }()
	before := liveHeap() + stackInUse()
	var version atomic.Int64
	shared := tableView("table", r.rows, &version)
	f, _ := shared.Render(ctx, nil)
	res := curveResult{frame: len(f.([]byte))}
	var sinks []*sizeSink
	var wg sync.WaitGroup
	wait := func(n int64) {
		for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(time.Millisecond) {
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
				t.Fatalf("the streams did not get %d writes", n)
			}
		}
	}
	bare := func() int {
		s := b.Stats().Streams
		return s["identity"] + s["zstd asleep"]
	}
	round := int64(0)
	for i := range r.streams {
		coding := sse.Brotli
		if r.mix == "zstd" || r.mix == "mixed" && i%2 == 0 {
			coding = sse.Zstd
		}
		v := shared
		if r.own {
			v = tableView("table:"+strconv.Itoa(i), r.rows, &version)
		}
		s := &sizeSink{header: http.Header{}}
		// The new stream's headers, then its first frame, then one frame
		// a round: every stream has 2 + round flushes once it caught up.
		s.flushes.Store(round)
		sinks = append(sinks, s)
		req := httptest.NewRequestWithContext(ctx, "GET", "/", nil)
		req.Header.Set("Accept-Encoding", coding)
		wasBare := bare()
		wg.Go(func() { h.Serve(s, req, live.Spec{Views: []live.View{v}}) })
		wait(2 + round)
		if res.firstBare == 0 && bare() > wasBare {
			res.firstBare = i + 1
		}
		if (i+1)%10 == 0 || i == r.streams-1 {
			round++
			for _, s := range sinks {
				s.last.Store(0)
			}
			version.Add(1)
			h.Changed([]string{"table"})
			wait(2 + round)
		}
	}
	st := b.Stats()
	res.opened, res.reserved, res.frames, res.tiers = r.streams, st.Reserved, st.Frames, st.Streams
	for k, n := range st.Streams {
		if k != "identity" && k != "zstd asleep" {
			res.coded += n
		}
		if holdsFrame(k, res.frame) {
			res.held += n
		}
	}
	var wire int64
	for _, s := range sinks {
		wire += s.last.Load()
	}
	res.perFrame = float64(wire) / float64(len(sinks))
	res.heap = liveHeap() + stackInUse() - before
	cancel()
	wg.Wait()
	<-ran
	return res
}

// sizeSink is a client that takes everything, counts its flushes and keeps
// the bytes of the last one.
type sizeSink struct {
	header  http.Header
	flushes atomic.Int64
	pending int64
	last    atomic.Int64
}

func (s *sizeSink) Header() http.Header { return s.header }
func (s *sizeSink) WriteHeader(int)     {}
func (s *sizeSink) Write(p []byte) (int, error) {
	s.pending += int64(len(p))
	return len(p), nil
}
func (s *sizeSink) Flush() {
	s.last.Store(s.pending)
	s.pending = 0
	s.flushes.Add(1)
}

// TestBudgetCurve opens streams one by one against budgets of 64, 128 and
// 150 MiB, as many as twice the MiB, brotli only, zstd only and half each,
// all watching one view of a data table of 50, 142 or 230 KB; and 150
// tabs that each watch a view of their own of 230 KB in 150 MiB. It writes
// curve.csv, and the docs' table of it to curve.md.
func TestBudgetCurve(t *testing.T) {
	request(t)
	var rows []curveRow
	for _, n := range []int{150, 430, 700} {
		for _, mib := range []int64{64, 128, 150} {
			for _, mix := range []string{"br", "zstd", "mixed"} {
				rows = append(rows, curveRow{rows: n, mib: mib, mix: mix, streams: int(mib) * 2})
			}
		}
	}
	for _, mix := range []string{"br", "zstd", "mixed"} {
		rows = append(rows, curveRow{rows: 700, mib: 150, mix: mix, streams: 150, own: true})
	}
	f, err := os.Create("curve.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := csv.NewWriter(f)
	c.Write([]string{"frame_bytes", "views", "limit_mib", "mix", "opened", "holding_frame", "compressed", "first_uncompressed",
		"booked_mib", "frames_mib", "heap_mib", "bytes_per_frame", "tiers"})
	var md strings.Builder
	md.WriteString("| Frame | Views | Budget | Clients | Opened | Window holds the frame | Compressed | First uncompressed | Booked | Heap | Bytes per frame |\n")
	md.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, r := range rows {
		res := curve(t, r)
		views := "one"
		if r.own {
			views = "one each"
		}
		var tiers []string
		for _, k := range slices.Sorted(maps.Keys(res.tiers)) {
			tiers = append(tiers, fmt.Sprintf("%s: %d", k, res.tiers[k]))
		}
		first := "none"
		if res.firstBare > 0 {
			first = ordinal(res.firstBare)
		}
		mib := func(n int64) string { return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) }
		c.Write([]string{strconv.Itoa(res.frame), views, strconv.FormatInt(r.mib, 10), r.mix, strconv.Itoa(res.opened), strconv.Itoa(res.held),
			strconv.Itoa(res.coded), strconv.Itoa(res.firstBare), mib(res.reserved), mib(res.frames), mib(res.heap),
			strconv.FormatFloat(res.perFrame, 'f', 0, 64), strings.Join(tiers, "; ")})
		c.Flush()
		clients := map[string]string{"br": "brotli", "zstd": "zstd", "mixed": "half each"}[r.mix]
		fmt.Fprintf(&md, "| %d KB | %s | %d MiB | %s | %d | %d | %d | %s | %s MiB | %s MiB | %s |\n",
			(res.frame+500)/1000, views, r.mib, clients, res.opened, res.held, res.coded, first, mib(res.reserved), mib(res.heap), thousands(int64(res.perFrame+0.5)))
		t.Logf("%6d B %-8s %3d MiB %-5s %3d opened, %3d hold the frame, %3d compressed, first bare %3d, booked %s MiB (frames %s), heap %s MiB, %.0f B/frame; %s",
			res.frame, views, r.mib, r.mix, res.opened, res.held, res.coded, res.firstBare, mib(res.reserved), mib(res.frames), mib(res.heap), res.perFrame, strings.Join(tiers, "; "))
	}
	if err := c.Error(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("curve.md", []byte(md.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ordinal writes n as 1st, 2nd, 3rd, 4th.
func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return strconv.Itoa(n) + suffix
}

// thousands writes n with commas between its thousands.
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
