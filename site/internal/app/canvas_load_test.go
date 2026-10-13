package app_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	goruntime "runtime" // runtime is a pattern in dashboard_test.go
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/app"
	"github.com/zweiundeins/lith/site/internal/app/apptest"
)

// TestCanvasLoad puts 100 painters on a running site: each one opens the
// canvas, its stream (zstd) and paints two pixels every 100 ms, 20 a
// second, the limit, from an address of its own. It reads the site's
// metrics before and after, and reports the process's memory and CPU,
// the writer's work and the bytes each stream received.
//
//	go tool task build
//	GOMEMLIMIT=192MiB ./lith-site -addr 127.0.0.1:18097 -db /tmp/canvas-load/site.db &
//	LITH_SITE_CANVAS_LOAD=http://127.0.0.1:18097 go test ./internal/app -run TestCanvasLoad -v
//
// LITH_SITE_CANVAS_METRICS defaults to the metrics listener beside it (the
// port + 100), LITH_SITE_CANVAS_PAINTERS to 100, LITH_SITE_CANVAS_FOR to
// 60s. LITH_SITE_CANVAS_LOAD=inproc runs the site in this process instead,
// with the memory limit of the unit (192 MiB), and reads the live heap
// after a collection every five seconds: what the process holds, without
// the garbage the collector lets the heap carry up to the limit. Its
// heap includes the painters' own few MiB.
func TestCanvasLoad(t *testing.T) {
	base := os.Getenv("LITH_SITE_CANVAS_LOAD")
	if base == "" {
		t.Skip("set LITH_SITE_CANVAS_LOAD to the URL of a running site, or to inproc")
	}
	var metrics func() map[string]float64
	inproc := base == "inproc"
	if inproc {
		defer debug.SetMemoryLimit(debug.SetMemoryLimit(192 << 20))
		var a *app.App
		a, base = apptest.Start(t)
		metrics = func() map[string]float64 {
			rec := httptest.NewRecorder()
			a.Metrics().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
			return canvasParseMetrics(rec.Body)
		}
	} else {
		metricsURL := os.Getenv("LITH_SITE_CANVAS_METRICS")
		if metricsURL == "" {
			host, port, _ := strings.Cut(strings.TrimPrefix(base, "http://"), ":")
			p, _ := strconv.Atoi(port)
			metricsURL = fmt.Sprintf("http://%s:%d/metrics", host, p+100)
		}
		metrics = func() map[string]float64 { return canvasScrape(t, metricsURL) }
	}
	painters := canvasEnvInt("LITH_SITE_CANVAS_PAINTERS", 100)
	duration := 60 * time.Second
	if d, err := time.ParseDuration(os.Getenv("LITH_SITE_CANVAS_FOR")); err == nil {
		duration = d
	}
	transport := &http.Transport{MaxIdleConnsPerHost: 4 * painters, DisableCompression: true}
	before := metrics()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var painted, refused, failed, streamed atomic.Int64
	var wg sync.WaitGroup
	for i := range painters {
		wg.Go(func() {
			jar, _ := cookiejar.New(nil)
			c := &http.Client{Jar: jar, Transport: transport}
			addr := fmt.Sprintf("10.9.%d.%d", i/250, 1+i%250)
			tab := fmt.Sprintf("load-painter-%03d-%d", i, time.Now().UnixNano())
			do := func(method, path, body string) (*http.Response, error) {
				req, _ := http.NewRequestWithContext(ctx, method, base+path, strings.NewReader(body))
				req.Header.Set("X-Forwarded-For", addr)
				if method == "POST" {
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Datastar-Request", "true")
					req.Header.Set("Sec-Fetch-Site", "same-origin")
				} else {
					req.Header.Set("Accept-Encoding", "zstd")
				}
				return c.Do(req)
			}
			res, err := do("GET", "/demos/canvas", "")
			if err != nil {
				failed.Add(1)
				return
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			stream, err := do("GET", "/demos/canvas/stream?tab="+tab, "")
			if err != nil || stream.StatusCode != http.StatusOK {
				failed.Add(1)
				return
			}
			go func() {
				defer stream.Body.Close()
				n, _ := io.Copy(io.Discard, stream.Body)
				streamed.Add(n)
			}()
			color := 1 + i%15
			post := func(path, body string) {
				res, err := do("POST", path, body)
				if err != nil {
					if ctx.Err() == nil {
						failed.Add(1)
					}
					return
				}
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
				switch res.StatusCode {
				case http.StatusNoContent:
					painted.Add(1)
				case http.StatusTooManyRequests:
					refused.Add(1)
				default:
					failed.Add(1)
				}
			}
			post("/cmd/canvas/pick", fmt.Sprintf(`{"tab":%q,"color":%d}`, tab, color))
			// A stroke wanders over the canvas, two cells a batch.
			x, y := rand.IntN(64), rand.IntN(64)
			step := func() int {
				x = (x + rand.IntN(3) + 63) % 64
				y = (y + rand.IntN(3) + 63) % 64
				return y*64 + x
			}
			tick := time.NewTicker(100 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					post("/cmd/canvas/paint", fmt.Sprintf(`{"tab":%q,"color":%d,"cells":[%d,%d]}`, tab, color, step(), step()))
				}
			}
		})
	}
	// Sample the process while the painters paint.
	var rss, heap, reserved, live float64
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		m := metrics()
		rss, heap = max(rss, m["process_resident_memory_bytes"]), max(heap, m["go_memstats_heap_inuse_bytes"])
		reserved = max(reserved, m["lith_site_sse_budget_reserved_bytes"])
		if inproc {
			goruntime.GC()
			var ms goruntime.MemStats
			goruntime.ReadMemStats(&ms)
			live = max(live, float64(ms.HeapAlloc))
		}
	}
	during := metrics()
	stop()
	wg.Wait()
	transport.CloseIdleConnections()

	secs := during["process_cpu_seconds_total"] - before["process_cpu_seconds_total"]
	commands := during["lith_site_writer_commands_total"] - before["lith_site_writer_commands_total"]
	batches := during["lith_site_writer_batches_total"] - before["lith_site_writer_batches_total"]
	busy := during["lith_site_writer_busy_seconds_total"] - before["lith_site_writer_busy_seconds_total"]
	frames := during["lith_site_live_frames_sent_total"] - before["lith_site_live_frames_sent_total"]
	t.Logf("%d painters for %v: %d actions answered 204, %d 429, %d failed", painters, duration, painted.Load(), refused.Load(), failed.Load())
	t.Logf("streams open: %.0f, tiers: %s", during["lith_site_live_streams"], canvasTiers(during))
	t.Logf("resident at most %.1f MiB, heap in use at most %.1f MiB, encoders' budget reserved at most %.1f MiB of %.1f MiB",
		rss/(1<<20), heap/(1<<20), reserved/(1<<20), during["lith_site_sse_budget_limit_bytes"]/(1<<20))
	if inproc {
		t.Logf("live heap after a collection at most %.1f MiB", live/(1<<20))
	}
	t.Logf("CPU: %.2f cores on average", secs/duration.Seconds())
	t.Logf("writer: %.0f commands in %.0f batches (%.1f a batch), busy %.1f%% of the time", commands, batches, commands/max(1, batches), 100*busy/duration.Seconds())
	t.Logf("frames sent: %.0f; wire: %.0f bytes a second per stream, %.0f bytes a frame", frames, float64(streamed.Load())/duration.Seconds()/float64(painters), float64(streamed.Load())/max(1, frames))
	if failed.Load() > 0 {
		t.Errorf("%d requests failed", failed.Load())
	}
}

func canvasEnvInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}

// scrape reads the metrics of a site's metrics listener.
func canvasScrape(t *testing.T, url string) map[string]float64 {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return canvasParseMetrics(res.Body)
}

// canvasParseMetrics reads Prometheus' text format into values by name, with
// the labels as written.
func canvasParseMetrics(r io.Reader) map[string]float64 {
	m := map[string]float64{}
	lines := bufio.NewScanner(r)
	lines.Buffer(nil, 1<<20)
	for lines.Scan() {
		line := lines.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			continue
		}
		name, value := line[:i], line[i+1:]
		if v, err := strconv.ParseFloat(value, 64); err == nil {
			m[name] = v
		}
	}
	return m
}

func canvasTiers(m map[string]float64) string {
	var parts []string
	for k, v := range m {
		if s, ok := strings.CutPrefix(k, "lith_site_sse_budget_streams{"); ok && v > 0 {
			parts = append(parts, fmt.Sprintf("%s %.0f", strings.TrimSuffix(s, "}"), v))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
