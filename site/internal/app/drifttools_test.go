package app_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/browser"
)

// This file holds what TestLifeDrift needs besides its loop: the page as
// served with or without newest-frame.js, a stream's Cache-Control as
// given, a link with a round trip time, a performance trace of the page,
// and the CPU time of the site and of Caddy.

// pluginMode rewrites the Game of Life's document for LIFE_DRIFT_PLUGIN:
// "reopen" (the page as it ships, data-newest-frame__reopen), "noreopen"
// (data-newest-frame: the page skips frames but never asks for a slow
// stream) or "off" (neither the attribute nor the script).
func pluginMode(mode string, h http.Handler) http.Handler {
	if mode == "" || mode == "reopen" {
		return h
	}
	script := regexp.MustCompile(`<script type="module" src="[^"]*newest-frame[^"]*"></script>`)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/demos/") || strings.HasSuffix(r.URL.Path, "/stream") {
			h.ServeHTTP(w, r)
			return
		}
		r.Header.Del("Accept-Encoding")
		rec := &recorder{header: http.Header{}, code: http.StatusOK}
		h.ServeHTTP(rec, r)
		body := rec.body.Bytes()
		if strings.HasPrefix(rec.header.Get("Content-Type"), "text/html") {
			switch mode {
			case "noreopen":
				body = bytes.ReplaceAll(body, []byte("data-newest-frame__reopen"), []byte("data-newest-frame"))
			case "off":
				body = bytes.ReplaceAll(body, []byte(" data-newest-frame__reopen"), nil)
				body = script.ReplaceAll(body, nil)
			}
			rec.header.Del("Content-Length")
		}
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.code)
		w.Write(body)
	})
}

type recorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(code int)        { r.code = code }
func (r *recorder) Write(p []byte) (int, error) { return r.body.Write(p) }

// cacheControl sets the stream's Cache-Control as its headers go out, for
// LIFE_DRIFT_CACHE: "" keeps what lith sends (no-store), "none" removes
// it, anything else replaces it.
func cacheControl(h http.Header, v string) {
	switch v {
	case "":
	case "none":
		h.Del("Cache-Control")
	default:
		h.Set("Cache-Control", v)
	}
}

// delayLink is a TCP proxy that holds every chunk in each direction for
// half the round trip time before passing it on, without a limit on the
// bandwidth: a fast link far away. DevTools' network emulation adds its
// latency to a response's start only, not to the frames of a stream that
// is open, so it cannot stand in for one. It counts the bytes each way.
type delayLink struct {
	oneWay   time.Duration
	up, down atomic.Int64 // bytes from the browser, and to it
}

// startDelayLink listens on a port of its own and forwards to target.
func startDelayLink(t *testing.T, target int, rtt time.Duration) (*delayLink, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	d := &delayLink{oneWay: rtt / 2}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(target))
			if err != nil {
				c.Close()
				continue
			}
			go d.pipe(c, s, &d.up)
			go d.pipe(s, c, &d.down)
		}
	}()
	return d, ln.Addr().(*net.TCPAddr).Port
}

func (d *delayLink) pipe(from, to net.Conn, n *atomic.Int64) {
	type chunk struct {
		b  []byte
		at time.Time
	}
	q := make(chan chunk, 1<<12)
	go func() {
		defer to.Close()
		for c := range q {
			time.Sleep(time.Until(c.at))
			if _, err := to.Write(c.b); err != nil {
				for range q {
				}
				return
			}
		}
	}()
	defer close(q)
	for {
		buf := make([]byte, 32<<10)
		k, err := from.Read(buf)
		if k > 0 {
			n.Add(int64(k))
			q <- chunk{buf[:k], time.Now().Add(d.oneWay)}
		}
		if err != nil {
			return
		}
	}
}

// traceMarks runs before the page's scripts when the test records a trace.
// It marks in the trace's User Timing track what the trace cannot tell by
// itself: each read of the stream's body with its length (lith:read
// <bytes>), each patch event as it arrives (lith:recv), and the time
// Datastar takes to apply each (lith:apply board for the board's frames).
// A capture listener on document runs before Datastar's own, and one added
// after it runs after; with newest-frame.js only the events it lets
// through reach document.
const traceMarks = `(() => {
  const P = performance
  let n = 0
  const tidy = () => { if (++n % 2000 === 0) { P.clearMarks(); P.clearMeasures() } }
  const patch = d => !!d && typeof d.type === 'string' && d.type.startsWith('datastar-patch')
  const board = ` + lifeBoardFrame + `
  const name = d => board(d) ? 'board' : d.type.slice(9)
  const read = ReadableStreamDefaultReader.prototype.read
  ReadableStreamDefaultReader.prototype.read = function () {
    return read.call(this).then(r => {
      if (r && r.value && r.value.byteLength) { P.mark('lith:read ' + r.value.byteLength); tidy() }
      return r
    })
  }
  const seen = new WeakSet() // newest-frame.js dispatches the events it applies again
  addEventListener('datastar-fetch', e => {
    if (!patch(e.detail) || seen.has(e.detail)) return
    seen.add(e.detail)
    P.mark('lith:recv ' + name(e.detail))
    tidy()
  }, true)
  let t0 = 0
  document.addEventListener('datastar-fetch', e => { if (patch(e.detail)) t0 = P.now() }, true)
  let on = false
  addEventListener('datastar-fetch', () => {
    if (on) return
    on = true
    document.addEventListener('datastar-fetch', e => {
      if (patch(e.detail)) { P.measure('lith:apply ' + name(e.detail), {start: t0, end: P.now()}); tidy() }
    })
  }, true)
})()`

// traceCategories are those DevTools' Performance panel records, with the
// CPU profiler's samples.
var traceCategories = []string{
	"-*", "devtools.timeline", "disabled-by-default-devtools.timeline", "disabled-by-default-devtools.timeline.frame",
	"disabled-by-default-devtools.timeline.stack", "v8.execute", "disabled-by-default-v8.cpu_profiler",
	"blink.user_timing", "loading", "toplevel", "latencyInfo", "__metadata",
}

// pageTrace records a trace of the page and writes it to path once it
// ends, as JSON that DevTools' Performance panel and ui.perfetto.dev open.
type pageTrace struct {
	p    *browser.Page
	path string
	done chan string // the stream handle of Tracing.tracingComplete
}

func newPageTrace(p *browser.Page, path string) *pageTrace {
	pt := &pageTrace{p: p, path: path, done: make(chan string, 1)}
	p.OnEvent("Tracing.tracingComplete", func(params json.RawMessage) {
		var e struct{ Stream string }
		json.Unmarshal(params, &e)
		pt.done <- e.Stream
	})
	return pt
}

func (pt *pageTrace) start(t testing.TB) {
	pt.p.Call(t, "Tracing.start", map[string]any{
		"transferMode": "ReturnAsStream",
		"streamFormat": "json",
		"traceConfig": map[string]any{
			"recordMode":         "recordAsMuchAsPossible",
			"includedCategories": traceCategories[1:],
			"excludedCategories": []string{"*"},
		},
	})
}

// end stops the trace and copies it to the file, and returns its size.
func (pt *pageTrace) end(t testing.TB) int64 {
	pt.p.Call(t, "Tracing.end", nil)
	var handle string
	select {
	case handle = <-pt.done:
	case <-time.After(2 * time.Minute):
		t.Fatal("no Tracing.tracingComplete")
	}
	f, err := os.Create(pt.path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var n int64
	for {
		var r struct {
			Data          string
			EOF           bool
			Base64Encoded bool
		}
		json.Unmarshal(pt.p.Call(t, "IO.read", map[string]any{"handle": handle, "size": 4 << 20}), &r)
		b := []byte(r.Data)
		if r.Base64Encoded {
			if b, err = base64.StdEncoding.DecodeString(r.Data); err != nil {
				t.Fatal(err)
			}
		}
		k, err := f.Write(b)
		n += int64(k)
		if err != nil {
			t.Fatal(err)
		}
		if r.EOF {
			break
		}
	}
	pt.p.Call(t, "IO.close", map[string]any{"handle": handle})
	return n
}

// goT is a test for a goroutine of its own: Fatal marks the test failed
// and ends the goroutine, where testing.T's would have to run on the
// test's.
type goT struct{ *testing.T }

func (g goT) Fatal(args ...any) {
	g.T.Error(args...)
	goruntime.Goexit()
}

func (g goT) Fatalf(format string, args ...any) {
	g.T.Errorf(format, args...)
	goruntime.Goexit()
}

// cpuTime is the CPU time this process (the site and the test) and the
// process pid (Caddy, 0 for none) have used.
func cpuTime(pid int) (self, other time.Duration) {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		self = time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	}
	if pid > 0 {
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
			s := string(b)
			if i := strings.LastIndexByte(s, ')'); i > 0 {
				fs := strings.Fields(s[i+1:])
				if len(fs) > 13 {
					u, _ := strconv.ParseInt(fs[11], 10, 64)
					k, _ := strconv.ParseInt(fs[12], 10, 64)
					other = time.Duration(u+k) * time.Second / 100 // USER_HZ
				}
			}
		}
	}
	return self, other
}

// requestMeter counts the requests besides the stream and the document
// that reach the site, such as a page's acknowledgements, with their
// bytes as HTTP/1.1 would carry them.
type requestMeter struct {
	mu    sync.Mutex
	n     int64
	bytes int64
}

func (m *requestMeter) add(r *http.Request) {
	n := int64(len(r.Method) + len(r.URL.RequestURI()) + 12)
	for k, vs := range r.Header {
		for _, v := range vs {
			n += int64(len(k) + len(v) + 4)
		}
	}
	if r.ContentLength > 0 {
		n += r.ContentLength
	}
	m.mu.Lock()
	m.n++
	m.bytes += n
	m.mu.Unlock()
}

func (m *requestMeter) read() (int64, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n, m.bytes
}
