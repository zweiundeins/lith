package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
	"github.com/zweiundeins/lith/site/internal/demos/life"
)

// TestLifeDrift measures how far a tab of the Game of Life falls behind the
// board when its main thread applies frames more slowly than they come, on
// an unthrottled link: a phone too slow for the page on a fast network. A
// fresh soup every 15 s keeps the board busy. Every second it compares the
// generation on the board with the one the page shows, and reads the
// frames the hub dropped, the bytes the site wrote to the stream, how long
// its writes blocked and what the sockets hold, and whether the page,
// with newest-frame.js, asked for a slow stream. LIFE_DRIFT is the CPU
// slowdown, LIFE_DRIFT_FOR how long (default 5m), LIFE_DRIFT_SLOW=1 has
// navigator.connection say 3g, so the page opens a slow stream from the
// start, and CADDY, the path of a caddy binary, puts Caddy with TLS and
// HTTP/2 between the browser and the site, as in production. It only
// reports, and writes every second's numbers to LIFE_DRIFT_OUT if set.
//
//	LIFE_DRIFT=10 CADDY=$(which caddy) LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestLifeDrift -v -timeout 30m
func TestLifeDrift(t *testing.T) {
	if os.Getenv("LIFE_DRIFT") == "" {
		t.Skip("set LIFE_DRIFT to the CPU slowdown to measure")
	}
	browser.SkipFirefox(t, "the Firefox driver cannot slow the CPU")
	rate, _ := strconv.ParseFloat(os.Getenv("LIFE_DRIFT"), 64)
	span := 5 * time.Minute
	if d, err := time.ParseDuration(os.Getenv("LIFE_DRIFT_FOR")); err == nil {
		span = d
	}
	slow := os.Getenv("LIFE_DRIFT_SLOW") == "1"

	a, _ := apptest.Start(t)
	// The site again on a port of its own, behind a writer that times the
	// stream's writes.
	var wm writeMeter
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: wm.wrap(a.Handler()), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	sitePort := ln.Addr().(*net.TCPAddr).Port
	base, front := "http://"+ln.Addr().String(), sitePort
	if caddy := os.Getenv("CADDY"); caddy != "" {
		front = freePort(t)
		base = "https://127.0.0.1:" + strconv.Itoa(front)
		startCaddy(t, caddy, front, sitePort)
	}

	b := browser.Start(t)
	p := b.NewPage(t, 1440, 900)
	p.Call(t, "Security.setIgnoreCertificateErrors", map[string]any{"ignore": true})
	var mu sync.Mutex
	streams := map[string]string{}
	var proto, coding string
	var received atomic.Int64
	p.OnEvent("Network.requestWillBeSent", func(params json.RawMessage) {
		var e struct {
			RequestID string
			Request   struct{ URL string }
		}
		json.Unmarshal(params, &e)
		if strings.Contains(e.Request.URL, "/stream?") {
			mu.Lock()
			streams[e.RequestID] = e.Request.URL
			mu.Unlock()
		}
	})
	p.OnEvent("Network.responseReceived", func(params json.RawMessage) {
		var e struct {
			RequestID string
			Response  struct {
				Protocol string
				Headers  map[string]string
			}
		}
		json.Unmarshal(params, &e)
		mu.Lock()
		if _, ok := streams[e.RequestID]; ok {
			proto = e.Response.Protocol
			for k, v := range e.Response.Headers {
				if strings.EqualFold(k, "content-encoding") {
					coding = v
				}
			}
		}
		mu.Unlock()
	})
	p.OnEvent("Network.dataReceived", func(params json.RawMessage) {
		var e struct {
			RequestID         string
			EncodedDataLength int64
		}
		json.Unmarshal(params, &e)
		mu.Lock()
		_, ok := streams[e.RequestID]
		mu.Unlock()
		if ok {
			received.Add(e.EncodedDataLength)
		}
	})
	p.Call(t, "Network.enable", nil)
	link := "4g"
	if slow {
		link = "3g"
	}
	p.Call(t, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": `Object.defineProperty(Navigator.prototype, 'connection', {get: () => ({effectiveType: '` + link + `', saveData: false})})`})
	if rate > 1 {
		p.Call(t, "Emulation.setCPUThrottlingRate", map[string]any{"rate": rate})
	}

	// The board's generation, read often, and when it reached each.
	var board boardClock
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go board.follow(ctx, func() (g int64) {
		a.DB().R.QueryRow(`SELECT generation FROM life_board`).Scan(&g)
		return g
	})

	p.Navigate(t, base+"/demos/life")
	p.Wait(t, `document.querySelector('.live-badge.is-live')`, 60*time.Second)
	p.Eval(t, lifeScrollToStage, nil)
	// A fresh soup every 15 s keeps the board as busy as a new one, without
	// the new soup that a quiet board gets, which counts from 0 again.
	stir := func() error {
		return a.Writer().Do(context.Background(), writer.Func(func(tx *writer.Tx) error {
			cells := make([]byte, life.Cells)
			life.Soup(cells, rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 1)))
			if _, err := tx.Exec(`UPDATE life_board SET cells = ?, quiet = 0, recent = x''`, cells); err != nil {
				return err
			}
			tx.Touch(life.Topic)
			return nil
		}))
	}
	if err := stir(); err != nil {
		t.Fatal(err)
	}
	go func() {
		tk := time.NewTicker(15 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				stir()
			}
		}
	}()
	// A frame the page applies changes the generation's text once.
	p.Eval(t, `window.applied_ = 0; new MutationObserver(rs => { const dd = document.querySelector('#life-stats dd'); for (const r of rs) if (r.target === dd || r.target.parentNode === dd) window.applied_++ }).observe(document.getElementById('life-stats'), {subtree: true, childList: true, characterData: true}); 0`, nil)

	var out *os.File
	if path := os.Getenv("LIFE_DRIFT_OUT"); path != "" {
		if out, err = os.Create(path); err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		fmt.Fprintln(out, "t,board,page,lag_gen,lag_s,applied,dropped,sent,renders,wrote,received,write_ms,write_max_ms,site_sendq,proxy_recvq,proxy_sendq,browser_recvq,streams")
	}
	start := time.Now()
	h0 := a.Hub().Stats()
	var lastApplied float64
	var lastWrite writeStats
	t.Logf("CPU %gx, %s, hint %v", rate, base, slow)
	t.Logf("| t | board | page | lag (gen) | lag (s) | applied/s | dropped | lith wrote KB | browser got KB | write ms (max) | queues KB: site send, proxy recv, proxy send, browser recv |")
	for i := 1; time.Since(start) < span; i++ {
		time.Sleep(time.Until(start.Add(time.Duration(i) * time.Second)))
		var shown struct {
			Gen     string
			Applied float64
		}
		p.Eval(t, `({gen: document.querySelector('#life-stats dd').textContent, applied: window.applied_})`, &shown)
		now := time.Since(start)
		g, _ := strconv.ParseInt(strings.ReplaceAll(shown.Gen, ",", ""), 10, 64)
		pg := g
		bg := board.now()
		lagS := board.since(pg)
		h := a.Hub().Stats()
		ws := wm.stats()
		q := queues(sitePort, front)
		row := []any{now.Seconds(), bg, pg, bg - pg, lagS.Seconds(), shown.Applied - lastApplied,
			h.FramesDropped - h0.FramesDropped, h.FramesSent - h0.FramesSent, h.Renders - h0.Renders,
			ws.bytes, received.Load(), float64(ws.blocked-lastWrite.blocked) / 1e6, float64(ws.max) / 1e6,
			q.siteSend, q.proxyRecv, q.proxySend, q.browserRecv, ws.streams}
		lastApplied, lastWrite = shown.Applied, ws
		wm.resetMax()
		if out != nil {
			var cells []string
			for _, c := range row {
				cells = append(cells, fmt.Sprint(c))
			}
			fmt.Fprintln(out, strings.Join(cells, ","))
		}
		if i%15 == 0 || i <= 3 {
			t.Logf("| %3.0f | %d | %d | %d | %.1f | %.1f | %d | %.0f | %.0f | %.0f (%.0f) | %.0f, %.0f, %.0f, %.0f |", now.Seconds(), bg, pg, bg-pg, lagS.Seconds(),
				row[5], row[6], float64(ws.bytes)/1000, float64(received.Load())/1000, row[11], row[12],
				float64(q.siteSend)/1000, float64(q.proxyRecv)/1000, float64(q.proxySend)/1000, float64(q.browserRecv)/1000)
		}
	}
	mu.Lock()
	ws := wm.stats()
	slowAfter := "never"
	if !ws.slowAt.IsZero() {
		slowAfter = fmt.Sprintf("after %.1f s", ws.slowAt.Sub(start).Seconds())
	}
	t.Logf("stream: %s, %s; %d stream requests; a slow stream (Save-Data) %s", proto, coding, ws.streams, slowAfter)
	mu.Unlock()
	if errs := p.Errors(); len(errs) > 0 {
		t.Logf("the page: %v", errs)
	}
}

// boardClock follows the board's generation.
type boardClock struct {
	mu     sync.Mutex
	times  []time.Time
	values []int64 // the generations reached, and when
}

func (c *boardClock) follow(ctx context.Context, read func() int64) {
	for ctx.Err() == nil {
		g := read()
		c.mu.Lock()
		if len(c.values) == 0 || g != c.values[len(c.values)-1] {
			c.values, c.times = append(c.values, g), append(c.times, time.Now())
		}
		c.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
}

// now is the board's generation.
func (c *boardClock) now() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.values) == 0 {
		return 0
	}
	return c.values[len(c.values)-1]
}

// since is how long ago the board passed generation g.
func (c *boardClock) since(g int64) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	i := sort.Search(len(c.values), func(i int) bool { return c.values[i] > g })
	if i >= len(c.values) {
		return 0
	}
	return time.Since(c.times[i])
}

// writeMeter times the writes and flushes of the site's stream responses.
type writeMeter struct {
	mu sync.Mutex
	s  writeStats
}

type writeStats struct {
	bytes   int64
	blocked time.Duration // in writes and flushes, in all
	max     time.Duration // the longest since resetMax
	streams int
	slowAt  time.Time // the first stream request with Save-Data: on
}

func (m *writeMeter) stats() writeStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s
}

func (m *writeMeter) resetMax() {
	m.mu.Lock()
	m.s.max = 0
	m.mu.Unlock()
}

func (m *writeMeter) add(n int, d time.Duration) {
	m.mu.Lock()
	m.s.bytes += int64(n)
	m.s.blocked += d
	m.s.max = max(m.s.max, d)
	m.mu.Unlock()
}

func (m *writeMeter) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/stream") {
			h.ServeHTTP(w, r)
			return
		}
		m.mu.Lock()
		m.s.streams++
		if m.s.slowAt.IsZero() && strings.EqualFold(r.Header.Get("Save-Data"), "on") {
			m.s.slowAt = time.Now()
		}
		m.mu.Unlock()
		h.ServeHTTP(&timedWriter{ResponseWriter: w, m: m}, r)
	})
}

type timedWriter struct {
	http.ResponseWriter
	m *writeMeter
}

func (w *timedWriter) Write(p []byte) (int, error) {
	t0 := time.Now()
	n, err := w.ResponseWriter.Write(p)
	w.m.add(n, time.Since(t0))
	return n, err
}

func (w *timedWriter) FlushError() error {
	t0 := time.Now()
	err := http.NewResponseController(w.ResponseWriter).Flush()
	w.m.add(0, time.Since(t0))
	return err
}

func (w *timedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// socketQueues are the bytes waiting in the kernel's buffers along the
// stream: what the site sent and its peer has not acknowledged, what the
// proxy received and has not read, the same between the proxy and the
// browser. Without a proxy, the browser's side is the site's peer.
type socketQueues struct {
	siteSend, proxyRecv, proxySend, browserRecv int64
}

// queues reads /proc/net/tcp and tcp6 for the connections to the site's
// port and to the port the browser talks to.
func queues(site, front int) (q socketQueues) {
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			fs := strings.Fields(line)
			if len(fs) < 5 || fs[3] != "01" { // established
				continue
			}
			port := func(addr string) int {
				i := strings.LastIndexByte(addr, ':')
				n, _ := strconv.ParseInt(addr[i+1:], 16, 32)
				return int(n)
			}
			tx, rx, _ := strings.Cut(fs[4], ":")
			txq, _ := strconv.ParseInt(tx, 16, 64)
			rxq, _ := strconv.ParseInt(rx, 16, 64)
			local, remote := port(fs[1]), port(fs[2])
			switch {
			case local == site:
				q.siteSend += txq
			case remote == site && front != site:
				q.proxyRecv += rxq
			case local == front:
				q.proxySend += txq
			}
			if remote == front {
				q.browserRecv += rxq
			}
		}
	}
	return q
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// startCaddy runs Caddy on 127.0.0.1:port with its own certificate (tls
// internal) in front of the site, as deploy/lith-site.caddy configures it.
func startCaddy(t *testing.T, bin string, port, site int) {
	t.Helper()
	dir := t.TempDir()
	conf := fmt.Sprintf(`{
	admin off
	auto_https disable_redirects
	skip_install_trust
	http_port %d
	https_port %d
	log {
		level ERROR
	}
}
https://127.0.0.1:%d {
	tls internal
	reverse_proxy 127.0.0.1:%d {
		header_up X-Forwarded-For {http.request.remote.host}
		header_up X-Real-IP {http.request.remote.host}
		flush_interval -1
	}
}
`, freePort(t), port, port, site)
	file := filepath.Join(dir, "Caddyfile")
	if err := os.WriteFile(file, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "run", "--config", file, "--adapter", "caddyfile")
	cmd.Env = append(os.Environ(), "HOME="+dir, "XDG_DATA_HOME="+dir, "XDG_CONFIG_HOME="+dir)
	var logs strings.Builder
	cmd.Stdout, cmd.Stderr = &lockedBuilder{b: &logs}, &lockedBuilder{b: &logs}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	deadline := time.Now().Add(20 * time.Second)
	for {
		c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("caddy did not start: %s", logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type lockedBuilder struct {
	mu sync.Mutex
	b  *strings.Builder
}

func (w *lockedBuilder) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}
