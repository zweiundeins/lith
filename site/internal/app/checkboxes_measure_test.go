package app_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	goruntime "runtime"
	"runtime/debug"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/app"
	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demos/checkboxes"
)

// TestMeasureCheckboxes measures the million checkboxes in process, with
// the memory limit of the production unit (GOMEMLIMIT 192 MiB, so the
// streams' encoders share 96 MiB): the flips a second the writer takes,
// the bytes of a frame, and the memory and CPU of 100 tabs scrolled to
// different places while people click.
//
//	LITH_SITE_MEASURE=1 go test ./internal/app -run TestMeasureCheckboxes -v
func TestMeasureCheckboxes(t *testing.T) {
	if os.Getenv("LITH_SITE_MEASURE") == "" {
		t.Skip("set LITH_SITE_MEASURE=1 to measure")
	}
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(192 << 20))
	a, base := apptest.Start(t)
	ctx := context.Background()
	cbFill(t, a)

	// The writer alone: people clicking at once, each click a flip.
	t.Log("## The writer")
	for _, people := range []int{1, 16, 128, 512} {
		before := a.Writer().Stats()
		var flips atomic.Int64
		deadline := time.Now().Add(3 * time.Second)
		var wg sync.WaitGroup
		for p := range people {
			wg.Go(func() {
				for i := 0; time.Now().Before(deadline); i++ {
					box := (p*7919 + i/2*104729) % checkboxes.Boxes
					if err := a.Writer().Do(ctx, checkboxes.SetBox{Box: box, Checked: i%2 == 0, At: time.Now()}); err != nil {
						t.Error(err)
						return
					}
					flips.Add(1)
				}
			})
		}
		wg.Wait()
		after := a.Writer().Stats()
		batches := after.Batches - before.Batches
		t.Logf("%4d people: %6d clicks a second, %5.1f a transaction, p99 commit %v", people, flips.Load()/3, float64(after.Commands-before.Commands)/float64(max(batches, 1)), after.P99Commit)
	}

	// A frame: the tile of a chunk a third full, the tab's list, the
	// numbers; and on a zstd stream, what a flip costs on the wire.
	t.Log("## Frames")
	tile, board, stats := cbFrameSizes(t, base)
	t.Logf("uncompressed: a tile %d bytes, the tab's list %d bytes, the numbers %d bytes", tile, board, stats)
	wire := cbMeasureWire(t, a, base)
	t.Logf("on a zstd stream: %s", wire)

	// 100 tabs scrolled to 100 places.
	t.Log("## 100 tabs")
	base0 := cbMem()
	streams := cbOpenTabs(t, a, base, 100)
	waitFor(t, "100 tabs on their windows", func() bool { return a.Hub().Stats().Views >= 100*checkboxes.Slots+100+1 })
	time.Sleep(time.Second)
	opened := cbMem()
	hs := a.Hub().Stats()
	bs := a.Hub().Budget().Stats()
	t.Logf("views %d, streams %d; encoders hold %.1f MiB of %.0f MiB: %v", hs.Views, hs.Streams, cbMiB(bs.Reserved), cbMiB(bs.Limit), bs.Streams)
	t.Logf("heap in use %.1f MiB (%.1f before), RSS %.1f MiB (%.1f before): %.2f MiB a tab", cbMiB(opened.heap), cbMiB(base0.heap), cbMiB(opened.rss), cbMiB(base0.rss), cbMiB(opened.heap-base0.heap)/100)

	// People click: a flip every 5 ms, in the windows of the tabs.
	t.Log("## Under load")
	if path := os.Getenv("LITH_SITE_PROFILE"); path != "" {
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer func() { pprof.StopCPUProfile(); f.Close() }()
	}
	for _, every := range []time.Duration{20 * time.Millisecond, 5 * time.Millisecond, time.Millisecond} {
		before, cpu0, wire0, sent0 := a.Hub().Stats(), cbCPU(), streams.bytes(), time.Now()
		stop := time.Now().Add(10 * time.Second)
		n := 0
		for tick := time.NewTicker(every); time.Now().Before(stop); <-tick.C {
			tab := n % 100
			box := tab*checkboxes.Chunks/100*checkboxes.ChunkSize + rand.IntN(checkboxes.Slots*checkboxes.ChunkSize)
			a.Writer().Send(checkboxes.SetBox{Box: min(box, checkboxes.Boxes-1), Checked: rand.IntN(2) == 0, At: time.Now()})
			n++
		}
		a.Writer().Flush(ctx)
		took := time.Since(sent0).Seconds()
		after, cpu := a.Hub().Stats(), cbCPU()-cpu0
		m := cbMem()
		t.Logf("%5.0f clicks a second: %.0f%% of a core, %d ticks (%d over 50 ms), %.0f frames a second, %.1f KiB a second on the wire to all tabs, heap %.1f MiB, RSS %.1f MiB",
			float64(n)/took, 100*cpu.Seconds()/took, after.Ticks-before.Ticks, after.Overruns-before.Overruns,
			float64(after.FramesSent-before.FramesSent)/took, float64(streams.bytes()-wire0)/1024/took, cbMiB(m.heap), cbMiB(m.rss))
	}

	// People scroll: every tab moves its window every two seconds, a few
	// tiles on, or once in five far away.
	t.Log("## Scrolling")
	{
		before, cpu0, wire0, sent0 := a.Hub().Stats(), cbCPU(), streams.bytes(), time.Now()
		moves := 0
		for round := range 5 {
			for i := range 100 {
				start := (i*(checkboxes.Chunks-checkboxes.Slots)/100 + (round+1)*4) % (checkboxes.Chunks - checkboxes.Slots)
				if (i+round)%5 == 0 {
					start = rand.IntN(checkboxes.Chunks - checkboxes.Slots)
				}
				a.Writer().Send(checkboxes.SetWindow{Viewer: demo.Viewer{Session: cbSessionID(i), Tab: fmt.Sprintf("tab%03d", i)}, Window: checkboxes.Window{Start: start}, At: time.Now(), Hub: a.Hub()})
				moves++
				time.Sleep(20 * time.Millisecond)
			}
		}
		a.Writer().Flush(ctx)
		time.Sleep(200 * time.Millisecond)
		took := time.Since(sent0).Seconds()
		after, cpu := a.Hub().Stats(), cbCPU()-cpu0
		m := cbMem()
		t.Logf("%.0f windows a second: %.0f%% of a core, %d renders, %d ticks (%d over 50 ms), %.0f frames a second, %.1f KiB a second on the wire to all tabs, %d views, heap %.1f MiB, RSS %.1f MiB",
			float64(moves)/took, 100*cpu.Seconds()/took, after.Renders-before.Renders, after.Ticks-before.Ticks, after.Overruns-before.Overruns,
			float64(after.FramesSent-before.FramesSent)/took, float64(streams.bytes()-wire0)/1024/took, after.Views, cbMiB(m.heap), cbMiB(m.rss))
	}
	streams.close()

	// The popular case: 100 tabs at the top, the same twelve chunks.
	t.Log("## 100 tabs at the top")
	waitFor(t, "the streams gone", func() bool { return a.Hub().Stats().Streams <= 1 })
	m0 := cbMem()
	top := cbOpenTabsAt(t, a, base, 100, func(int) int { return 0 })
	waitFor(t, "100 tabs at the top", func() bool { return a.Hub().Count(checkboxes.ChunkTopic(0)) >= 100 })
	time.Sleep(time.Second)
	m1 := cbMem()
	hs = a.Hub().Stats()
	t.Logf("views %d; heap in use %.1f MiB (%.1f before)", hs.Views, cbMiB(m1.heap), cbMiB(m0.heap))
	{
		before, cpu0, wire0, sent0 := a.Hub().Stats(), cbCPU(), top.bytes(), time.Now()
		n := 0
		for tick := time.NewTicker(20 * time.Millisecond); time.Since(sent0) < 10*time.Second; <-tick.C {
			a.Writer().Send(checkboxes.SetBox{Box: rand.IntN(checkboxes.Slots * checkboxes.ChunkSize), Checked: rand.IntN(2) == 0, At: time.Now()})
			n++
		}
		a.Writer().Flush(ctx)
		took := time.Since(sent0).Seconds()
		after, cpu := a.Hub().Stats(), cbCPU()-cpu0
		t.Logf("%.0f clicks a second, all in view of all 100: %.0f%% of a core, %d over 50 ms, %.0f frames a second, %.1f KiB a second on the wire to all tabs",
			float64(n)/took, 100*cpu.Seconds()/took, after.Overruns-before.Overruns, float64(after.FramesSent-before.FramesSent)/took, float64(top.bytes()-wire0)/1024/took)
	}
	top.close()
}

// cbFill checks about a third of the boxes, straight in the chunks.
func cbFill(t *testing.T, a *app.App) {
	t.Helper()
	err := a.Writer().Do(context.Background(), writer.Func(func(tx *writer.Tx) error {
		bits := make([]byte, checkboxes.ChunkSize/8)
		on := 0
		for n := range checkboxes.Chunks {
			on = 0
			for i := range bits {
				x, y := byte(rand.IntN(256)), byte(rand.IntN(256))
				bits[i] = x & y
				for b := bits[i]; b != 0; b &= b - 1 {
					on++
				}
			}
			if _, err := tx.Exec(`UPDATE checkboxes_chunks SET bits = ? WHERE id = ?`, bits, n); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE checkboxes_totals SET checked = checked + ?`, on); err != nil {
				return err
			}
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
}

// cbFrameSizes reads a tab's first frames off a stream without compression
// and returns the sizes of a tile's, the tab's list's and the numbers'
// events.
func cbFrameSizes(t *testing.T, base string) (tile, board, stats int) {
	t.Helper()
	c := client(t)
	events := listen(t, c, base+"/demos/checkboxes/stream?tab=sizes")
	post(t, c, base+"/cmd/checkboxes/window", `{"tab":"sizes","offset":0,"count":12,"cols":2}`)
	for tile == 0 || board == 0 || stats == 0 {
		var e string
		select {
		case e = <-events:
		case <-time.After(5 * time.Second):
			t.Fatal("no frames")
		}
		switch {
		case strings.Contains(e, `id="checkboxes-tile-`):
			tile = len(e)
		case strings.Contains(e, `id="checkboxes-board"`):
			board = len(e)
		case strings.Contains(e, `id="checkboxes-stats"`):
			stats = len(e)
		}
	}
	return tile, board, stats
}

type cbMemStat struct{ heap, rss int64 }

func cbMem() cbMemStat {
	goruntime.GC()
	var m goruntime.MemStats
	goruntime.ReadMemStats(&m)
	return cbMemStat{heap: int64(m.HeapInuse), rss: cbRSS()}
}

// cbRSS reads the process's resident memory.
func cbRSS() int64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			var kb int64
			fmt.Sscan(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "VmRSS:"), "kB")), &kb)
			return kb << 10
		}
	}
	return 0
}

func cbCPU() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func cbMiB(n int64) float64 { return float64(n) / (1 << 20) }

// cbTabs are open streams that count the bytes they receive.
type cbTabs struct {
	n      atomic.Int64
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (s *cbTabs) bytes() int64 { return s.n.Load() }

func (s *cbTabs) close() {
	s.cancel()
	s.wg.Wait()
}

// cbOpenTabs opens n streams with zstd, each a tab of its own session, and
// moves each to a window of its own, spread over the grid.
func cbOpenTabs(t *testing.T, a *app.App, base string, n int) *cbTabs {
	t.Helper()
	return cbOpenTabsAt(t, a, base, n, func(i int) int { return i * (checkboxes.Chunks - checkboxes.Slots) / n })
}

// cbOpenTabsAt opens n streams with zstd, tab i at the window that starts
// at chunk at(i).
func cbOpenTabsAt(t *testing.T, a *app.App, base string, n int, at func(int) int) *cbTabs {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &cbTabs{cancel: cancel}
	for i := range n {
		sid := cbSessionID(i)
		tab := fmt.Sprintf("tab%03d", i)
		req, _ := http.NewRequestWithContext(ctx, "GET", base+"/demos/checkboxes/stream?tab="+tab, nil)
		req.Header.Set("Accept-Encoding", "zstd")
		req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("stream %d: %v %v", i, err, res)
		}
		s.wg.Go(func() {
			defer res.Body.Close()
			buf := make([]byte, 16<<10)
			for {
				k, err := res.Body.Read(buf)
				s.n.Add(int64(k))
				if err != nil {
					return
				}
			}
		})
		start := at(i)
		err = a.Writer().Do(context.Background(), checkboxes.SetWindow{
			Viewer: demo.Viewer{Session: sid, Tab: tab}, Window: checkboxes.Window{Start: start}, At: time.Now(), Hub: a.Hub(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func cbSessionID(i int) string {
	b := make([]byte, 32)
	copy(b, fmt.Sprintf("measure-%d", i))
	return base64.RawURLEncoding.EncodeToString(b)
}

// cbMeasureWire opens a zstd stream, flips boxes in its window one a tick,
// and returns what each tick's frames cost on the wire.
func cbMeasureWire(t *testing.T, a *app.App, base string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sid := cbSessionID(1000)
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/demos/checkboxes/stream?tab=wire", nil)
	req.Header.Set("Accept-Encoding", "zstd")
	req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.Header.Get("Content-Encoding") != "zstd" {
		t.Fatalf("stream: %v %v", err, res.Header)
	}
	var raw atomic.Int64
	counted := cbReaderFunc(func(p []byte) (int, error) {
		n, err := res.Body.Read(p)
		raw.Add(int64(n))
		return n, err
	})
	// The page answers the list's first frame with its window: the top.
	if err := a.Writer().Do(context.Background(), checkboxes.SetWindow{
		Viewer: demo.Viewer{Session: sid, Tab: "wire"}, Window: checkboxes.Window{}, At: time.Now(), Hub: a.Hub(),
	}); err != nil {
		t.Fatal(err)
	}
	dec, err := zstd.NewReader(counted, zstd.WithDecoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	tiles := make(chan int64, 1024)
	go func() {
		lines := bufio.NewScanner(dec)
		lines.Buffer(nil, 1<<20)
		for lines.Scan() {
			if strings.Contains(lines.Text(), `id="checkboxes-tile-`) {
				tiles <- raw.Load()
			}
		}
	}()
	// The window's twelve tiles.
	var firsts int64
	for range checkboxes.Slots {
		select {
		case firsts = <-tiles:
		case <-time.After(5 * time.Second):
			t.Fatal("no tiles")
		}
	}
	time.Sleep(200 * time.Millisecond)
	from := raw.Load()
	const flips = 100
	for i := range flips {
		a.Writer().Do(context.Background(), checkboxes.SetBox{Box: i % (checkboxes.Slots * checkboxes.ChunkSize), Checked: i/(checkboxes.Slots*checkboxes.ChunkSize)%2 == 0, At: time.Now()})
		time.Sleep(60 * time.Millisecond) // one a tick
	}
	time.Sleep(200 * time.Millisecond)
	per := float64(raw.Load()-from) / flips
	return fmt.Sprintf("the first frames of a window (12 tiles, the list, the numbers) %d bytes; a flip, its tile and the numbers, %.0f bytes", firsts, per)
}

type cbReaderFunc func([]byte) (int, error)

func (f cbReaderFunc) Read(p []byte) (int, error) { return f(p) }

var _ io.Reader = cbReaderFunc(nil)
