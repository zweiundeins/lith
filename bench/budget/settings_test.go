package budget

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/zweiundeins/lith/sse"
)

// setting is one way to compress a stream.
type setting struct {
	coding string // sse.Zstd, sse.Brotli, sse.Gzip, or "" for none
	window int    // base 2 logarithm
	lowMem bool   // zstd's history at its window
	q      int    // brotli's quality
	dense  bool   // zstd at its default level instead of its fastest
}

func (s setting) String() string {
	switch s.coding {
	case sse.Zstd:
		name := "zstd"
		if s.dense {
			name = "zstd default"
		}
		if s.lowMem {
			return fmt.Sprintf("%s 2^%d low memory", name, s.window)
		}
		return fmt.Sprintf("%s 2^%d", name, s.window)
	case sse.Brotli:
		return fmt.Sprintf("brotli q%d 2^%d", s.q, s.window)
	case sse.Gzip:
		return "gzip"
	}
	return "identity"
}

func (s setting) options() sse.Options {
	switch s.coding {
	case sse.Zstd:
		o := sse.Options{ZstdWindow: s.window, ZstdLowMemory: s.lowMem}
		if s.dense {
			o.ZstdLevel = zstd.SpeedDefault
		}
		return o
	case sse.Brotli:
		return sse.Options{BrotliWindow: s.window, BrotliQuality: s.q}
	case sse.Gzip:
		return sse.Options{}
	}
	return sse.Options{NoCompression: true}
}

func (s setting) request() *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Encoding", cmpOr(s.coding, "identity"))
	return r
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// settings are the rows of the table: zstd at its fastest level with
// windows of 2^15 to 2^20, with its full history and with low memory;
// brotli at quality 3 and 4 with windows of 2^16 to 2^20; gzip; none.
func settings() []setting {
	var s []setting
	for w := 15; w <= 20; w++ {
		s = append(s, setting{coding: sse.Zstd, window: w}, setting{coding: sse.Zstd, window: w, lowMem: true})
	}
	for _, q := range []int{3, 4} {
		for w := 16; w <= 20; w++ {
			s = append(s, setting{coding: sse.Brotli, window: w, q: q})
		}
	}
	return append(s, setting{coding: sse.Gzip}, setting{})
}

// sink is a client that takes everything at once. It keeps the size of
// each flush when sizes is set.
type sink struct {
	header  http.Header
	pending int
	n       int64
	sizes   []int
	keep    bool
}

func (s *sink) Header() http.Header { return s.header }
func (s *sink) WriteHeader(int)     {}

func (s *sink) Write(p []byte) (int, error) {
	s.pending += len(p)
	return len(p), nil
}

func (s *sink) Flush() {
	if s.pending == 0 {
		return
	}
	s.n += int64(s.pending)
	if s.keep {
		s.sizes = append(s.sizes, s.pending)
	}
	s.pending = 0
}

// result is what one setting cost on one workload.
type result struct {
	workload string
	setting
	streams, frames int
	heap            float64 // bytes per stream after the session
	first           int     // bytes of the first frame on the wire
	perFrame        float64 // mean bytes per frame on the wire, after the first
	in              float64 // mean bytes per frame before compression
	cpu, wall       []time.Duration
}

// measure opens streams with the setting and writes every frame to each,
// frame by frame as the hub does. Then it measures the heap that stays in
// use while the streams are still open, divided by their number.
func measure(t testing.TB, workload string, frames [][]byte, s setting, streams int) result {
	t.Helper()
	before := liveHeap()
	sinks := make([]*sink, streams)
	open := make([]*sse.Stream, streams)
	for i := range open {
		sinks[i] = &sink{header: http.Header{}, keep: i == 0}
		st, err := sse.Open(sinks[i], s.request(), s.options())
		if err != nil {
			t.Fatal(err)
		}
		if st.Encoding() != s.coding {
			t.Fatalf("%v: the stream uses %q", s, st.Encoding())
		}
		open[i] = st
	}
	cpu0, start := cpuTime(), time.Now()
	for _, f := range frames {
		for _, st := range open {
			if err := st.Write(f); err != nil {
				t.Fatal(err)
			}
		}
	}
	wall, cpu := time.Since(start), cpuTime()-cpu0
	heap := float64(liveHeap()-before) / float64(streams)
	n := len(frames) * streams
	r := result{workload: workload, setting: s, streams: streams, frames: len(frames), heap: heap,
		cpu: []time.Duration{cpu / time.Duration(n)}, wall: []time.Duration{wall / time.Duration(n)}}
	sizes := sinks[0].sizes
	r.first = sizes[0]
	rest := 0
	for _, n := range sizes[1:] {
		rest += n
	}
	r.perFrame = float64(rest) / float64(len(sizes)-1)
	in := 0
	for _, f := range frames {
		in += len(f)
	}
	r.in = float64(in) / float64(len(frames))
	for _, st := range open {
		st.Close()
	}
	return r
}

func liveHeap() int64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func loadavg() string {
	b, _ := os.ReadFile("/proc/loadavg")
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return "?"
	}
	return strings.Join(f[:3], " ")
}

func request(t *testing.T) {
	t.Helper()
	if os.Getenv("LITH_BUDGET") == "" {
		t.Skip("set LITH_BUDGET=1 to measure")
	}
}

func runs() int {
	n, _ := strconv.Atoi(os.Getenv("LITH_BUDGET_RUNS"))
	return max(1, n)
}

// median is the middle of d and its range.
func median(d []time.Duration) (mid, lo, hi time.Duration) {
	d = slices.Sorted(slices.Values(d))
	return d[len(d)/2], d[0], d[len(d)-1]
}

const mib = 1 << 20

// TestSettings measures every setting on both workloads, 64 streams each,
// and writes settings.csv. LITH_BUDGET_RUNS repeats the runs; the CPU is
// then the median, with its range.
func TestSettings(t *testing.T) {
	request(t)
	const streams = 64
	workloads := []struct {
		name   string
		frames [][]byte
	}{
		{"board", boardFrames(t, 200)},
		{"page", pageFrames(150, 1)},
	}
	for _, w := range workloads {
		t.Logf("%s: %s", w.name, sizes(w.frames))
	}
	t.Logf("GOMAXPROCS %d, load average %s", runtime.GOMAXPROCS(0), loadavg())
	var results []result
	for run := range runs() {
		for _, w := range workloads {
			for _, s := range settings() {
				r := measure(t, w.name, w.frames, s, streams)
				if run == 0 {
					results = append(results, r)
					continue
				}
				k := slices.IndexFunc(results, func(x result) bool { return x.workload == w.name && x.setting == s })
				results[k].cpu = append(results[k].cpu, r.cpu...)
				results[k].wall = append(results[k].wall, r.wall...)
			}
		}
		t.Logf("run %d done, load average %s", run+1, loadavg())
	}
	f, err := os.Create("settings.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := csv.NewWriter(f)
	c.Write([]string{"workload", "setting", "coding", "window", "low_memory", "quality", "streams", "frames",
		"heap_mib_per_stream", "first_frame_bytes", "bytes_per_frame", "ratio", "cpu_us_per_frame", "cpu_us_min", "cpu_us_max", "wall_us_per_frame"})
	for _, r := range results {
		cpu, lo, hi := median(r.cpu)
		wall, _, _ := median(r.wall)
		us := func(d time.Duration) string { return strconv.FormatFloat(float64(d)/1e3, 'f', 1, 64) }
		c.Write([]string{r.workload, r.setting.String(), cmpOr(r.coding, "identity"), strconv.Itoa(r.window), strconv.FormatBool(r.lowMem), strconv.Itoa(r.q),
			strconv.Itoa(r.streams), strconv.Itoa(r.frames),
			strconv.FormatFloat(r.heap/mib, 'f', 3, 64), strconv.Itoa(r.first), strconv.FormatFloat(r.perFrame, 'f', 0, 64),
			strconv.FormatFloat(r.in/r.perFrame, 'f', 1, 64), us(cpu), us(lo), us(hi), us(wall)})
		t.Logf("%-5s %-24s %6.3f MiB  first %6d B  %6.0f B/frame  %5.1f:1  %7.1f us CPU (%.1f to %.1f)  %7.1f us wall",
			r.workload, r.setting, r.heap/mib, r.first, r.perFrame, r.in/r.perFrame, float64(cpu)/1e3, float64(lo)/1e3, float64(hi)/1e3, float64(wall)/1e3)
	}
	c.Flush()
	if err := c.Error(); err != nil {
		t.Fatal(err)
	}
}

// TestFloor measures zstd and brotli below the windows of the table, down
// to their smallest, on the page workload: what a compressed stream holds
// whatever its window. It writes floor.csv.
func TestFloor(t *testing.T) {
	request(t)
	const streams = 64
	frames := pageFrames(150, 1)
	var rows []setting
	for w := 10; w <= 16; w++ {
		rows = append(rows, setting{coding: sse.Zstd, window: w, lowMem: true})
	}
	for _, q := range []int{2, 3} {
		for w := 10; w <= 16; w++ {
			rows = append(rows, setting{coding: sse.Brotli, window: w, q: q})
		}
	}
	rows = append(rows, setting{coding: sse.Gzip}, setting{})
	t.Logf("load average %s", loadavg())
	f, err := os.Create("floor.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := csv.NewWriter(f)
	c.Write([]string{"setting", "coding", "window", "low_memory", "quality", "heap_mib_per_stream", "first_frame_bytes", "bytes_per_frame", "cpu_us_per_frame"})
	for _, s := range rows {
		r := measure(t, "page", frames, s, streams)
		c.Write([]string{s.String(), cmpOr(s.coding, "identity"), strconv.Itoa(s.window), strconv.FormatBool(s.lowMem), strconv.Itoa(s.q),
			strconv.FormatFloat(r.heap/mib, 'f', 3, 64), strconv.Itoa(r.first), strconv.FormatFloat(r.perFrame, 'f', 0, 64),
			strconv.FormatFloat(float64(r.cpu[0])/1e3, 'f', 1, 64)})
		t.Logf("%-24s %6.3f MiB  first %6d B  %6.0f B/frame  %6.1f us", s, r.heap/mib, r.first, r.perFrame, float64(r.cpu[0])/1e3)
	}
	c.Flush()
	t.Logf("load average %s", loadavg())
}
