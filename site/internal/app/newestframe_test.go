package app_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/app"
	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
)

// TestNewestFrame opens the Game of Life, which steps ten times a second,
// with newest-frame.js in the page. At full speed the page applies every
// frame of the board. When each frame costs its main thread 300 ms more,
// frames queue, and the page drops the older ones: each frame it applies
// is the newest that has arrived. Once it drops most of them, the page
// posts /pace: the hub counts the report (live.Hub.Behind) and paces the
// stream, and the page acknowledges each write it gets (live.Hub.Ack).
//
//	LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestNewestFrame
func TestNewestFrame(t *testing.T) {
	a, base := apptest.Start(t)
	b := browser.Start(t)
	p := b.NewPage(t, 1280, 900)
	p.AddInitScript(t, newestFrameProbe)
	p.Navigate(t, base+"/demos/life")
	p.Wait(t, `document.querySelector('.live-badge.is-live')`, 30*time.Second)
	freshSoup(t, a)

	time.Sleep(4 * time.Second)
	full := frameStats(t, p)
	if full.Applied < 25 || full.Dropped > 0 || full.Stale > 0 {
		t.Errorf("at full speed, the board's frames: %+v", full)
	}
	p.Eval(t, `window.__nf.cost = 300`, nil)
	time.Sleep(5 * time.Second)
	slow := frameStats(t, p)
	if slow.Applied-full.Applied < 5 || slow.Dropped == 0 || slow.Stale > 0 || slow.Old > 0 {
		t.Errorf("at 300 ms a frame, the board's frames: %+v", slow)
	}
	for deadline := time.Now().Add(5 * time.Second); a.Hub().Stats().Acks == 0 && time.Now().Before(deadline); {
		time.Sleep(100 * time.Millisecond)
	}
	if st := a.Hub().Stats(); st.Behind == 0 || st.BehindStreams != 1 || st.Paces != 1 || st.Acks == 0 {
		t.Errorf("the hub counted %d reports of %d streams behind, paced %d and took %d acknowledgements, want 1 stream paced", st.Behind, st.BehindStreams, st.Paces, st.Acks)
	}
	t.Logf("%s: %+v at full speed, %+v at 300 ms a frame", p.Engine(), full, slow)
	if errs := p.Errors(); len(errs) > 0 {
		t.Errorf("the page: %v", errs)
	}
}

// TestNewestFrameSlowStream opens the Game of Life with __reopen and
// without pacing on a page whose main thread each frame costs 300 ms
// more: it falls behind,
// newest-frame.js drops more than half of the frames and opens the stream
// again, at the same URL with Save-Data: on. The browser aborts the first request, the
// site answers with a slow stream, no frame of the first stream is applied
// after, and the page stays within a few seconds of the board.
//
//	LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestNewestFrameSlowStream
func TestNewestFrameSlowStream(t *testing.T) {
	a, _ := apptest.Start(t, func(c *app.Config) { c.Pacing = "off" })
	var streams streamLog
	base := serveSite(t, streams.wrap(pluginMode("reopen", a.Handler())))
	b := browser.Start(t)
	p := b.NewPage(t, 1280, 900)
	aborted := abortedStreams(t, p)
	p.AddInitScript(t, newestFrameProbe+"\nwindow.__nf.cost = 300")
	var board boardClock
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go board.follow(ctx, func() (g int64) {
		a.DB().R.QueryRow(`SELECT generation FROM life_board`).Scan(&g)
		return g
	})
	p.Navigate(t, base+"/demos/life")
	p.Wait(t, `document.querySelector('.live-badge.is-live')`, 30*time.Second)
	freshSoup(t, a)

	deadline := time.Now().Add(20 * time.Second)
	for len(streams.list()) < 2 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	switched := time.Now()
	before := frameStats(t, p)
	time.Sleep(4 * time.Second)
	after := frameStats(t, p)
	var gen string
	p.Eval(t, `document.querySelector('#life-stats dd').textContent`, &gen)
	g, _ := strconv.ParseInt(strings.ReplaceAll(gen, ",", ""), 10, 64)
	behind := board.since(g)
	list := streams.list()
	if len(list) != 2 {
		t.Fatalf("stream requests: %+v", list)
	}
	first, second := list[0], list[1]
	if first.url != second.url || first.saveData || !second.saveData {
		t.Errorf("the first stream %s, Save-Data %v; the second %s, Save-Data %v", first.url, first.saveData, second.url, second.saveData)
	}
	if got := aborted(); len(got) != 1 || !strings.HasSuffix(got[0], first.url) {
		t.Errorf("the browser aborted %q, not the first stream %s", got, first.url)
	}
	if first.end.IsZero() || first.end.Sub(second.start).Abs() > 2*time.Second {
		t.Errorf("the first stream: ended %v, %v after the second started", !first.end.IsZero(), first.end.Sub(second.start))
	}
	if !second.end.IsZero() {
		t.Error("the slow stream ended")
	}
	// Four frames a second on a slow stream; the page applies each
	// frame's newest, and nothing of the stream it left.
	rate := float64(after.Arrived-before.Arrived) / time.Since(switched).Seconds()
	if rate > 5 || after.Applied == before.Applied || after.Stale > 0 || after.Old > 0 || behind > 3*time.Second {
		t.Errorf("after the switch, %.1f frames a second, %v behind the board: %+v, then %+v", rate, behind, before, after)
	}
	t.Logf("%s: switched %v after the first stream opened, %v behind the board 4 s later; %+v, then %+v", p.Engine(), second.start.Sub(first.start).Round(100*time.Millisecond), behind.Round(10*time.Millisecond), before, after)
	if errs := p.Errors(); len(errs) > 0 {
		t.Errorf("the page: %v", errs)
	}
}

// newestFrameProbe runs before the page's scripts. Its capture listener on
// window, added before newest-frame.js adds its own, numbers the board's
// frames as they arrive from the stream; its listener on document, added
// after Datastar's, notes each frame Datastar applies, whether a newer one
// had arrived (stale) and whether it came from a stream opened again since
// (old). window.__nf.cost, in ms, makes every frame applied cost the main
// thread that much more: a slower CPU, alike in every engine.
const newestFrameProbe = `(() => {
  const nf = window.__nf = {arrived: 0, applied: [], stale: 0, old: 0, cost: 0}
  const seen = new WeakMap()
  const board = d => !!d && d.type === 'datastar-patch-elements' && d.el && d.el.id === 'stream' && typeof (d.argsRaw || {}).elements === 'string' && d.argsRaw.elements.includes('id="life-board"')
  addEventListener('datastar-fetch', e => {
    if (board(e.detail) && !seen.has(e.detail)) seen.set(e.detail, {n: ++nf.arrived, el: e.detail.el})
  }, true)
  let on = false
  addEventListener('datastar-fetch', () => {
    if (on) return
    on = true
    document.addEventListener('datastar-fetch', e => {
      const f = seen.get(e.detail)
      if (!f) return
      nf.applied.push(f.n)
      if (f.n !== nf.arrived) nf.stale++
      if (!f.el.isConnected) nf.old++
      const end = performance.now() + nf.cost
      while (performance.now() < end) {}
    })
  }, true)
})()`

// boardFrames counts the board's frames on a page with newestFrameProbe:
// those that arrived, those applied, and those dropped, which arrived
// before the newest applied one and were never applied.
type boardFrames struct {
	Arrived, Applied, Dropped, Stale, Old int
}

func frameStats(t *testing.T, p *browser.Page) (f boardFrames) {
	t.Helper()
	p.Eval(t, `(() => {
		const nf = window.__nf
		const got = new Set(nf.applied)
		const top = nf.applied.reduce((a, b) => Math.max(a, b), 0)
		let dropped = 0
		for (let n = 1; n <= top; n++) if (!got.has(n)) dropped++
		return {arrived: nf.arrived, applied: nf.applied.length, dropped, stale: nf.stale, old: nf.old}
	})()`, &f)
	return f
}

// abortedStreams follows the page's requests and returns the URLs of the
// stream requests the browser aborted so far: Chromium's
// Network.loadingFailed, cancelled, or Firefox's network.fetchError
// NS_BINDING_ABORTED.
func abortedStreams(t *testing.T, p *browser.Page) func() []string {
	t.Helper()
	var mu sync.Mutex
	urls := map[string]string{}
	var aborted []string
	p.OnEvent("Network.requestWillBeSent", func(params json.RawMessage) {
		var e struct {
			RequestID string
			Request   struct{ URL string }
		}
		json.Unmarshal(params, &e)
		if strings.Contains(e.Request.URL, "/stream?") {
			mu.Lock()
			urls[e.RequestID] = e.Request.URL
			mu.Unlock()
		}
	})
	p.OnEvent("Network.loadingFailed", func(params json.RawMessage) {
		var e struct {
			RequestID string
			Canceled  bool
		}
		json.Unmarshal(params, &e)
		mu.Lock()
		if u, ok := urls[e.RequestID]; ok && e.Canceled {
			aborted = append(aborted, u)
		}
		mu.Unlock()
	})
	p.OnEvent("network.fetchError", func(params json.RawMessage) {
		var e struct {
			ErrorText string
			Request   struct{ URL string }
		}
		json.Unmarshal(params, &e)
		if strings.Contains(e.Request.URL, "/stream?") && strings.Contains(e.ErrorText, "ABORT") {
			mu.Lock()
			aborted = append(aborted, e.Request.URL)
			mu.Unlock()
		}
	})
	p.Call(t, "Network.enable", nil)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), aborted...)
	}
}

// freshSoup gives the Game of Life a fresh soup, which steps ten times a
// second.
func freshSoup(t *testing.T, a *app.App) {
	t.Helper()
	if err := a.Writer().Do(context.Background(), writer.Func(func(tx *writer.Tx) error {
		_, err := tx.Exec(`UPDATE life_board SET cells = zeroblob(4000), quiet = 1`)
		return err
	})); err != nil {
		t.Fatal(err)
	}
}

// serveSite serves h on a port of its own until the test ends, and returns
// its base URL.
func serveSite(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return "http://" + ln.Addr().String()
}

// streamLog records the stream requests that reach the site.
type streamLog struct {
	mu   sync.Mutex
	reqs []*streamRequest
}

type streamRequest struct {
	url        string
	saveData   bool
	start, end time.Time // end is zero while the stream is open
}

func (l *streamLog) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/stream") {
			h.ServeHTTP(w, r)
			return
		}
		req := &streamRequest{url: r.URL.String(), saveData: strings.EqualFold(r.Header.Get("Save-Data"), "on"), start: time.Now()}
		l.mu.Lock()
		l.reqs = append(l.reqs, req)
		l.mu.Unlock()
		h.ServeHTTP(w, r)
		l.mu.Lock()
		req.end = time.Now()
		l.mu.Unlock()
	})
}

func (l *streamLog) list() []streamRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]streamRequest, len(l.reqs))
	for i, r := range l.reqs {
		out[i] = *r
	}
	return out
}
