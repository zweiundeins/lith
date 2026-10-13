package app_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// TestBoardLoad measures the shared board on the site's binary, in a
// process of its own with the production unit's GOMEMLIMIT: 100 tabs on
// the board, each from an address of its own and with a stream that
// accepts zstd, as Chromium's do, and one of them moving a card a second.
// It reports the server's CPU and resident memory from /proc, and the
// bytes each tab receives, first idle, then under the moves.
//
//	LITH_SITE_LOAD=1 go test ./internal/app -run TestBoardLoad -v
func TestBoardLoad(t *testing.T) {
	if os.Getenv("LITH_SITE_LOAD") == "" {
		t.Skip("set LITH_SITE_LOAD=1 to measure the board under load")
	}
	tabs, idle, moving := boardEnvInt("LITH_SITE_LOAD_TABS", 100), boardEnvDur("LITH_SITE_LOAD_IDLE", 20*time.Second), boardEnvDur("LITH_SITE_LOAD_FOR", 60*time.Second)
	dir := t.TempDir()
	bin := filepath.Join(dir, "lith-site")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/lith-site").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	port := boardFreePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	srv := exec.Command(bin, "-addr", fmt.Sprintf("127.0.0.1:%d", port), "-metrics", fmt.Sprintf("127.0.0.1:%d", boardFreePort(t)), "-db", filepath.Join(dir, "site.db"), "-log", "text")
	srv.Env = append(os.Environ(), "GOMEMLIMIT=192MiB")
	var logs bytes.Buffer
	srv.Stderr = &logs
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Process.Signal(os.Interrupt)
		srv.Wait()
		if t.Failed() {
			t.Logf("the server's log:\n%s", logs.String())
		}
	})
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if res, err := http.Get(base + "/healthz"); err == nil {
			res.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the server did not start")
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type tab struct {
		c    *http.Client
		addr string
		id   string
		wire atomic.Int64
	}
	// The second tab, which never moves, notes when each read arrives and
	// how many bytes it brought, to tell a move's frame from the others.
	type chunk struct {
		at time.Time
		n  int
	}
	var chunksMu sync.Mutex
	var chunks []chunk
	// The first tab also decodes its stream, to tell its events apart.
	var events boardBuffer
	decoded := make(chan struct{})
	list := make([]*tab, tabs)
	var wg sync.WaitGroup
	for i := range list {
		jar, _ := cookiejar.New(nil)
		tb := &tab{c: &http.Client{Jar: jar, Transport: &http.Transport{DisableCompression: true}}, addr: fmt.Sprintf("10.9.%d.%d", i/250, i%250+1), id: fmt.Sprintf("loadtab%03d", i)}
		list[i] = tb
		req, _ := http.NewRequest("GET", base+"/demos/board", nil)
		req.Header.Set("X-Real-IP", tb.addr)
		res, err := tb.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		req, _ = http.NewRequestWithContext(ctx, "GET", base+"/demos/board/stream?tab="+tb.id, nil)
		req.Header.Set("X-Real-IP", tb.addr)
		req.Header.Set("Accept-Encoding", "zstd, br, gzip")
		res, err = tb.c.Do(req)
		if err != nil || res.StatusCode != http.StatusOK || res.Header.Get("Content-Encoding") != "zstd" {
			t.Fatalf("stream %d: %v %v %q", i, err, res.StatusCode, res.Header.Get("Content-Encoding"))
		}
		wg.Go(func() {
			defer res.Body.Close()
			var body io.Reader = res.Body
			if i == 0 {
				pr, pw := io.Pipe()
				body = io.TeeReader(res.Body, pw)
				go func() {
					defer close(decoded)
					d, err := zstd.NewReader(pr)
					if err != nil {
						return
					}
					defer d.Close()
					io.Copy(&events, d)
					io.Copy(io.Discard, pr)
				}()
				defer pw.Close()
			}
			buf := make([]byte, 32<<10)
			for {
				n, err := body.Read(buf)
				tb.wire.Add(int64(n))
				if i == 1 && n > 0 {
					chunksMu.Lock()
					chunks = append(chunks, chunk{time.Now(), n})
					chunksMu.Unlock()
				}
				if err != nil {
					return
				}
			}
		})
		if code := boardPost(t, tb.c, tb.addr, base+"/cmd/board/hello", `{"tab":"`+tb.id+`"}`); code != http.StatusNoContent {
			t.Fatalf("hello %d: %d", i, code)
		}
	}
	// Every stream open and its first frames sent, and the server's own
	// start done: it compresses its static files at brotli -11 at first.
	time.Sleep(15 * time.Second)

	type sample struct {
		at   time.Time
		cpu  float64 // seconds
		wire int64
	}
	take := func() sample {
		var w int64
		for _, tb := range list {
			w += tb.wire.Load()
		}
		return sample{time.Now(), boardCPU(t, srv.Process.Pid), w}
	}
	report := func(name string, a, b sample, moves int) {
		secs := b.at.Sub(a.at).Seconds()
		cores := (b.cpu - a.cpu) / secs
		perTab := float64(b.wire-a.wire) / float64(tabs) / secs
		line := fmt.Sprintf("%s: %.0f s, %d tabs: %.2f%% of a core, %.0f bytes a second to each tab", name, secs, tabs, cores*100, perTab)
		if moves > 0 {
			line += fmt.Sprintf("; %d moves: %.0f bytes to each tab a move, %.2f ms of CPU a move", moves, float64(b.wire-a.wire)/float64(tabs)/float64(moves), (b.cpu-a.cpu)/float64(moves)*1000)
		}
		t.Log(line)
	}

	s0 := take()
	time.Sleep(idle)
	s1 := take()
	report("idle", s0, s1, 0)

	// One tab moves a card a second: each card in turn to the next lane,
	// at the top, from the version it had after its last move.
	lanes := []int{0, 0, 0, 0, 0, 1, 1, 1, 2, 2, 2, 2}
	versions := make([]int, 12)
	mover := list[0]
	moves := 0
	var committed []time.Time
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for end := time.Now().Add(moving); time.Now().Before(end); <-tick.C {
		card := moves % 12
		lanes[card] = (lanes[card] + 1) % 3
		body := fmt.Sprintf(`{"tab":"%s","op":"m%d","card":%d,"lane":%d,"index":0,"seen":%d}`, mover.id, moves, card+1, lanes[card], versions[card])
		if code := boardPost(t, mover.c, mover.addr, base+"/cmd/board/move", body); code != http.StatusNoContent {
			t.Fatalf("move %d: %d", moves, code)
		}
		committed = append(committed, time.Now())
		versions[card]++
		moves++
	}
	s2 := take()
	report("a move a second", s1, s2, moves)

	// What reached the second tab within 60 ms of each move's commit: the
	// tick that rendered the move, and now and then the numbers' frame of
	// the same tick.
	chunksMu.Lock()
	var perMove []int
	for _, c := range committed {
		sum := 0
		for _, ch := range chunks {
			if !ch.at.Before(c) && ch.at.Sub(c) <= 60*time.Millisecond {
				sum += ch.n
			}
		}
		perMove = append(perMove, sum)
	}
	chunksMu.Unlock()
	slices.Sort(perMove)
	t.Logf("a move's write to a tab that did not move: %d bytes at the median, %d to %d", perMove[len(perMove)/2], perMove[0], perMove[len(perMove)-1])

	m := boardMetrics(t, srv)
	rss := boardRSS(t, srv.Process.Pid)
	t.Logf("server: %.1f MiB resident, %.1f MiB heap in use, encoders hold %s of a %s budget; streams by tier: %s",
		float64(rss)/(1<<20), m["go_memstats_heap_inuse_bytes"]/(1<<20), boardMiB(m["lith_site_sse_budget_reserved_bytes"]), boardMiB(m["lith_site_sse_budget_limit_bytes"]), boardTiers(m))
	cancel()
	wg.Wait()
	<-decoded
	plain := events.String()
	count := map[string]int{}
	size := map[string]int{}
	for _, ev := range strings.Split(plain, "\n\n") {
		kind := "other"
		for _, k := range []string{"board-lanes", "board-stats", "board-here", "board-outcome", `"_board":{"v"`, `"_board":{"ack"`} {
			if strings.Contains(ev, k) {
				kind = k
				break
			}
		}
		count[kind]++
		size[kind] += len(ev) + 2
	}
	t.Logf("tab 0: %d bytes of events from %d on the wire (%.1f:1); events by kind, count and mean size: %s", len(plain), list[0].wire.Load(), float64(len(plain))/float64(max(1, list[0].wire.Load())), boardKinds(count, size))
}

type boardBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *boardBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *boardBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func boardKinds(count, size map[string]int) string {
	var parts []string
	for k, n := range count {
		parts = append(parts, fmt.Sprintf("%s %d × %d B", k, n, size[k]/max(1, n)))
	}
	return strings.Join(parts, ", ")
}

func boardPost(t *testing.T, c *http.Client, addr, url, body string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Datastar-Request", "true")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("X-Real-IP", addr)
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res.StatusCode
}

func boardFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// boardCPU is the process's user and system time in seconds.
func boardCPU(t *testing.T, pid int) float64 {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+2:]))
	utime, _ := strconv.ParseFloat(f[11], 64)
	stime, _ := strconv.ParseFloat(f[12], 64)
	return (utime + stime) / 100 // USER_HZ
}

// boardRSS is the process's resident memory in bytes.
func boardRSS(t *testing.T, pid int) int64 {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			kb, _ := strconv.ParseInt(strings.Fields(line)[1], 10, 64)
			return kb << 10
		}
	}
	return 0
}

// boardMetrics reads the server's /metrics, from the address after -metrics.
func boardMetrics(t *testing.T, srv *exec.Cmd) map[string]float64 {
	t.Helper()
	addr := ""
	for i, a := range srv.Args {
		if a == "-metrics" {
			addr = srv.Args[i+1]
		}
	}
	res, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	m := map[string]float64{}
	lines := bufio.NewScanner(res.Body)
	for lines.Scan() {
		line := lines.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			continue
		}
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err == nil {
			m[line[:i]] = v
		}
	}
	return m
}

func boardTiers(m map[string]float64) string {
	var parts []string
	for k, v := range m {
		if strings.HasPrefix(k, "lith_site_sse_budget_streams{") && v > 0 {
			parts = append(parts, strings.TrimPrefix(k, "lith_site_sse_budget_streams")+" "+strconv.FormatFloat(v, 'f', 0, 64))
		}
	}
	return strings.Join(parts, ", ")
}

func boardMiB(v float64) string { return strconv.FormatFloat(v/(1<<20), 'f', 1, 64) + " MiB" }

func boardEnvInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}

func boardEnvDur(name string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(name)); err == nil && d > 0 {
		return d
	}
	return def
}
