package life

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo/demotest"
)

// TestLoad measures the demo on a running site: it opens LIFE_LOAD_STREAMS
// streams on the board (100), as many tabs would, each asking for zstd,
// and reports over LIFE_LOAD_SECONDS (30) the generations, the frames and
// bytes each stream got, the server's CPU per generation and its memory.
// The site runs as in production, in a process of its own:
//
//	go build -o /tmp/lith-site ./cmd/lith-site
//	GOMEMLIMIT=192MiB /tmp/lith-site -addr 127.0.0.1:18097 -db /tmp/life.db &
//	LIFE_LOAD_URL=http://127.0.0.1:18097 LIFE_LOAD_PID=$! go test ./internal/demos/life -run TestLoad -v
func TestLoad(t *testing.T) {
	base := os.Getenv("LIFE_LOAD_URL")
	if base == "" {
		t.Skip("set LIFE_LOAD_URL to a running site, and LIFE_LOAD_PID to its process")
	}
	streams, seconds := envInt("LIFE_LOAD_STREAMS", 100), envInt("LIFE_LOAD_SECONDS", 30)
	pid := os.Getenv("LIFE_LOAD_PID")
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Transport: &http.Transport{MaxIdleConnsPerHost: streams, DisableCompression: true}}
	if res, err := c.Get(base + "/demos/life"); err != nil {
		t.Fatal(err)
	} else {
		res.Body.Close()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wire, plain, frames atomic.Int64
	var gens atomic.Int64 // the generations the first stream saw
	var wg sync.WaitGroup
	for i := range streams {
		wg.Go(func() {
			seen := &gens
			if i > 0 {
				seen = nil
			}
			if err := follow(ctx, c, base+"/demos/life/stream?tab=load"+strconv.Itoa(i), &wire, &plain, &frames, seen); err != nil && ctx.Err() == nil {
				t.Errorf("stream %d: %v", i, err)
			}
		})
	}
	time.Sleep(5 * time.Second) // every stream open, the budget settled
	w0, p0, f0, g0, cpu0 := wire.Load(), plain.Load(), frames.Load(), gens.Load(), cpuTicks(t, pid)
	start := time.Now()
	time.Sleep(time.Duration(seconds) * time.Second)
	took := time.Since(start)
	w1, p1, f1, g1, cpu1 := wire.Load(), plain.Load(), frames.Load(), gens.Load(), cpuTicks(t, pid)
	rss := procStatus(t, pid, "VmRSS")
	metrics := scrape(t, base)
	cancel()
	wg.Wait()

	steps, n := g1-g0, f1-f0
	t.Logf("%d streams for %v: %d generations, %.1f a second", streams, took.Round(time.Second), steps, float64(steps)/took.Seconds())
	if n > 0 {
		t.Logf("frames per stream: %.1f a second; per frame and stream: %d bytes on the wire (zstd), %d bytes of events", float64(n)/float64(streams)/took.Seconds(), (w1-w0)/n, (p1-p0)/n)
	}
	if pid != "" && steps > 0 {
		cpu := float64(cpu1-cpu0) / 100 // clock ticks
		t.Logf("server CPU: %.2f s in %v, %.2f cores, %.2f ms per generation", cpu, took.Round(time.Second), cpu/took.Seconds(), cpu*1000/float64(steps))
		t.Logf("server memory: VmRSS %s", rss)
	}
	for _, m := range []string{"go_memstats_heap_inuse_bytes", "go_memstats_heap_alloc_bytes", "go_memstats_next_gc_bytes", "go_memstats_sys_bytes", "go_gc_duration_seconds_count", "lith_site_sse_budget_limit_bytes", "lith_site_sse_budget_reserved_bytes", "lith_site_sse_budget_streams", "lith_site_live_frames_dropped_total", "lith_site_live_streams "} {
		for _, l := range metrics {
			if strings.HasPrefix(l, m) {
				t.Log(l)
			}
		}
	}
}

// follow reads one stream until ctx ends, counting its bytes on the wire
// and decompressed, its board frames, and if gens is not nil the
// generations they showed (a new soup starts again from 0).
func follow(ctx context.Context, c *http.Client, url string, wire, plain, frames, gens *atomic.Int64) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept-Encoding", "zstd")
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Encoding") != "zstd" {
		return fmt.Errorf("status %d, encoding %q", res.StatusCode, res.Header.Get("Content-Encoding"))
	}
	d, err := zstd.NewReader(&counter{r: res.Body, n: wire}, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
	if err != nil {
		return err
	}
	defer d.Close()
	events := bufio.NewReaderSize(d, 128<<10)
	var event bytes.Buffer
	last := int64(-1)
	for {
		line, err := events.ReadSlice('\n')
		plain.Add(int64(len(line)))
		event.Write(line)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return err
		}
		if len(line) > 1 {
			continue
		}
		if m := generationDD.FindSubmatch(event.Bytes()); m != nil {
			frames.Add(1)
			g, _ := strconv.ParseInt(strings.ReplaceAll(string(m[1]), ",", ""), 10, 64)
			if gens != nil && g != last {
				gens.Add(1)
				last = g
			}
		}
		event.Reset()
	}
}

var generationDD = regexp.MustCompile(`<dt>Generation</dt><dd>([0-9,]+)</dd>`)

type counter struct {
	r io.Reader
	n *atomic.Int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}

// cpuTicks is the process's user and system time, in clock ticks.
func cpuTicks(t *testing.T, pid string) int64 {
	if pid == "" {
		return 0
	}
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+2:]))
	utime, _ := strconv.ParseInt(fields[11], 10, 64)
	stime, _ := strconv.ParseInt(fields[12], 10, 64)
	return utime + stime
}

func procStatus(t *testing.T, pid, field string) string {
	if pid == "" {
		return ""
	}
	b, err := os.ReadFile("/proc/" + pid + "/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, field+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// scrape reads the site's metrics, on the public port + 100.
func scrape(t *testing.T, base string) []string {
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	res, err := http.Get("http://" + u.Hostname() + ":" + strconv.Itoa(port+100) + "/metrics")
	if err != nil {
		t.Log(err)
		return nil
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return strings.Split(string(b), "\n")
}

// BenchmarkStep is the clock's command on a soup, through the writer: a
// read of the board, the next generation, and the write, committed.
func BenchmarkStep(b *testing.B) {
	e := demotest.Open(b, Demo)
	at := time.UnixMilli(1_800_000_000_000)
	e.Do(b, Step{At: at})
	b.ResetTimer()
	for i := range b.N {
		at = at.Add(100 * time.Millisecond)
		if err := e.W.Do(context.Background(), Step{At: at, Watching: 1}); err != nil {
			b.Fatal(err)
		}
		if i%500 == 499 { // keep it a soup
			e.Do(b, writer.Func(func(tx *writer.Tx) error {
				brd, err := load(tx.Context(), tx)
				if err != nil {
					return err
				}
				brd.reseed(at.UnixMilli())
				return brd.save(tx)
			}))
		}
	}
}

// BenchmarkFrame renders the board's view: the HTML of the board, the
// counters and the shares, as one event.
func BenchmarkFrame(b *testing.B) {
	brd := Board{Cells: make([]byte, Cells), History: "[1,2,3]"}
	Soup(brd.Cells, rand.New(rand.NewPCG(1, 2)))
	now := time.Now()
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := frameEvent(ctx, brd, now); err != nil {
			b.Fatal(err)
		}
	}
}

func frameEvent(ctx context.Context, b Board, now time.Time) ([]byte, error) {
	var h strings.Builder
	if err := Frame(b, now).Render(ctx, &h); err != nil {
		return nil, err
	}
	return sse.PatchElements([]byte(h.String())), nil
}

// BenchmarkCompress is a stream's share of a generation: one frame
// through a zstd encoder that has the frames before it, at the site's
// settings. It reports the bytes each frame takes on the wire, with every
// cell the same length and with dead cells as the shorter <p>.
func BenchmarkCompress(b *testing.B) {
	for _, markup := range []string{"fixed", "short"} {
		for _, low := range []bool{false, true} {
			name := fmt.Sprintf("%s/window=19", markup)
			window := 19
			if low {
				name, window = fmt.Sprintf("%s/window=18,lowmem", markup), 18
			}
			b.Run(name, func(b *testing.B) {
				frames := soupFrames(b, 300, markup == "short")
				var out bytes.Buffer
				enc, err := zstd.NewWriter(&out, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(1<<window),
					zstd.WithEncoderConcurrency(1), zstd.WithLowerEncoderMem(low), zstd.WithEncoderCRC(false))
				if err != nil {
					b.Fatal(err)
				}
				for _, f := range frames[:20] { // a history, as a stream has
					enc.Write(f)
					enc.Flush()
				}
				out.Reset()
				n := 0
				b.ResetTimer()
				for i := range b.N {
					enc.Write(frames[20+i%(len(frames)-20)])
					enc.Flush()
					n++
				}
				b.ReportMetric(float64(out.Len())/float64(n), "wire-bytes/frame")
				b.ReportMetric(float64(len(frames[len(frames)-1])), "frame-bytes")
			})
		}
	}
}

// soupFrames renders n generations of a soup with people's patterns in
// it, as board view events.
func soupFrames(b *testing.B, n int, short bool) [][]byte {
	rng := rand.New(rand.NewPCG(7, 9))
	cur := make([]byte, Cells)
	Soup(cur, rng)
	for i, p := range Patterns {
		p.Stamp(cur, rng.IntN(Cells), i, byte(1+i))
	}
	next := make([]byte, Cells)
	var frames [][]byte
	for g := range n + 100 {
		Next(next, cur, int64(g))
		cur, next = next, cur
		if g < 100 { // past the soup's first rush
			continue
		}
		brd := Board{Cells: cur, Generation: int64(g), History: "[]"}
		ev, err := frameEvent(context.Background(), brd, time.Now())
		if err != nil {
			b.Fatal(err)
		}
		if short {
			ev = bytes.ReplaceAll(ev, []byte("<p class=_>"), []byte("<p>"))
		}
		frames = append(frames, ev)
	}
	return frames
}
