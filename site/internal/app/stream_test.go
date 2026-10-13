package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
)

// TestStreamReports reads the inspector's stream over HTTP: the tab's
// table, its choices, and with every frame a report from the stream itself,
// here uncompressed. A change of size answers 204 and arrives on the
// stream with its op id; the landing page's stream reports on itself too.
func TestStreamReports(t *testing.T) {
	_, base := apptest.Start(t)
	c := client(t)
	get(t, c, base+"/demos/stream")

	events := listen(t, c, base+"/demos/stream/stream?tab=tab1")
	next(t, events, `"_stream":{"size":"large","churn":"calm","ack":""}`)
	next(t, events, `id="stream-payload"`)
	first := next(t, events, `"_stream":{"n":`)
	for _, want := range []string{`"coding":"none"`, `"tier":"uncompressed"`, `"heap":"none"`, `"streams":1`} {
		if !strings.Contains(first, want) {
			t.Errorf("the first report has no %s:\n%s", want, first)
		}
	}
	next(t, events, `"n":3,`)

	if code := post(t, c, base+"/cmd/stream/payload", `{"tab":"tab1","op":"op1","size":"small"}`); code != http.StatusNoContent {
		t.Fatalf("a smaller table: %d", code)
	}
	next(t, events, `"ack":"op1"`)
	table := next(t, events, `16 rows, 96 cells`)
	if n := strings.Count(table, "<tr>"); n != 17 {
		t.Errorf("the small table has %d rows", n-1)
	}
	if code := post(t, c, base+"/cmd/stream/payload", `{"tab":"tab1","op":"op2","size":"huge"}`); code != http.StatusBadRequest {
		t.Errorf("a size that does not exist: %d", code)
	}

	home := listen(t, c, base+"/stream?tab=tab2")
	if r := next(t, home, `"_stream":{"n":`); !strings.Contains(r, `"coding":"none"`) {
		t.Errorf("the landing page's report:\n%s", r)
	}
}

// TestStreamInspector opens the stream inspector in Chromium: the readout
// fills in from the stream's own reports and keeps moving, the size control
// changes the bytes of every frame, and a second tab shows up in the first
// one's count of open streams. The landing page's slot reads its own
// stream.
//
//	LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestStreamInspector
func TestStreamInspector(t *testing.T) {
	_, base := apptest.Start(t)
	b := browser.Start(t)
	text := func(p *browser.Page, sel string) string {
		var s string
		p.Eval(t, `document.querySelector(`+jsString(sel)+`).textContent.trim()`, &s)
		return s
	}
	click := func(p *browser.Page, label string) {
		p.Eval(t, `[...document.querySelectorAll('.sx-choice button')].find(b => b.textContent === `+jsString(label)+`).click()`, nil)
	}

	ada := b.NewPage(t, 1280, 900)
	ada.Navigate(t, base+"/demos/stream")
	ada.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)
	ada.Wait(t, `document.querySelector('.sx-coding').textContent === 'zstd'`, 5*time.Second)
	ada.Wait(t, `document.querySelector('.sx-pairs code').textContent.startsWith('zstd 2^')`, 5*time.Second)
	ada.Wait(t, `document.querySelector('.sx-heroes.is-ready')`, 5*time.Second)
	if n := text(ada, ".sx-note"); !strings.HasPrefix(n, "256 rows") {
		t.Errorf("a new tab's table: %q", n)
	}

	// The numbers move: frames keep coming, each with a report, and each
	// lights up the cells it changed.
	sent := text(ada, ".sx-tiles dd")
	ada.Wait(t, `document.querySelector('.sx-tiles dd').textContent !== `+jsString(sent), 5*time.Second)
	ada.Wait(t, `document.querySelectorAll('#stream-payload td[data-new]').length > 0`, 5*time.Second)
	ada.Wait(t, `document.querySelector('.sx-spark').data?.length > 8`, 5*time.Second)
	var fps float64
	ada.Eval(t, `new Promise(r => setTimeout(() => r(parseFloat(document.querySelector('.sx-pace span:last-child b').textContent)), 4000))`, &fps)
	if fps < 3 || fps > 5 {
		t.Errorf("%.1f frames a second, want about 4", fps)
	}

	// A smaller table: the bytes of events in each frame drop, from kilobytes
	// to a few thousand bytes.
	if large := text(ada, ".sx-bar b"); !strings.HasSuffix(large, " KB") {
		t.Errorf("the large table's frame: %s", large)
	}
	click(ada, "Small")
	ada.Wait(t, `document.querySelectorAll('#stream-payload tbody tr').length === 16`, 5*time.Second)
	ada.Wait(t, `[...document.querySelectorAll('.sx-choice button')].find(b => b.textContent === 'Small').getAttribute('aria-pressed') === 'true'`, 5*time.Second)
	ada.Wait(t, `!document.querySelector('.sx-choice button[aria-busy="true"]')`, 5*time.Second)
	ada.Wait(t, `(t => t.endsWith(' B') && parseInt(t.replace(/,/g, '')) < 4000)(document.querySelector('.sx-bar b').textContent)`, 5*time.Second)

	// A second tab: the first counts it among the site's streams.
	bob := b.NewPage(t, 1280, 900)
	bob.Navigate(t, base+"/demos/stream")
	bob.Wait(t, `document.querySelector('.sx-heroes.is-ready')`, 10*time.Second)
	ada.Wait(t, `Number(document.querySelector('.sx-streams b').textContent) >= 2`, 5*time.Second)

	home := b.NewPage(t, 1280, 900)
	home.Navigate(t, base+"/")
	home.Wait(t, `document.querySelector('[data-slot="hypermedia"] .sx-embed .sx-coding').textContent === 'zstd'`, 10*time.Second)
	home.Wait(t, `/^[0-9,.]+ (B|KB)$/.test(document.querySelector('.sx-embed__nums div:last-child dd').textContent)`, 10*time.Second)

	for name, p := range map[string]*browser.Page{"Ada's inspector": ada, "Bob's inspector": bob, "the landing page": home} {
		if errs := p.Errors(); len(errs) > 0 {
			t.Errorf("%s: %v", name, errs)
		}
	}
}

// TestShotsStream writes screenshots of the stream inspector, its landing
// slot, and a frame after the payload went still, with the memory limit of
// the deploy, so the budget has a limit to show:
//
//	LITH_SITE_SHOTS=/tmp/shots LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestShotsStream
func TestShotsStream(t *testing.T) {
	dir := os.Getenv("LITH_SITE_SHOTS")
	if dir == "" {
		t.Skip("set LITH_SITE_SHOTS to a directory for the screenshots")
	}
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(192 << 20))
	_, base := apptest.Start(t)
	b := browser.Start(t)
	shot := func(name string, w, h int, cookie string, setup func(*browser.Page), shoot func(*browser.Page)) {
		p := b.NewPage(t, w, h)
		defer p.Close(t)
		if cookie != "" {
			p.Navigate(t, base+"/")
			p.Eval(t, "document.cookie = '"+cookie+"; Path=/'", nil)
		}
		p.Navigate(t, base+"/demos/stream")
		p.Eval(t, "document.fonts.ready.then(() => true)", nil)
		p.Wait(t, `document.querySelector('.sx-heroes.is-ready')`, 10*time.Second)
		if setup != nil {
			setup(p)
		}
		time.Sleep(4 * time.Second) // the sparkline fills, the numbers settle
		shoot(p)
		if errs := p.Errors(); len(errs) > 0 {
			t.Errorf("%s: %v", name, errs)
		}
	}
	stage := func(name string) func(*browser.Page) {
		return func(p *browser.Page) {
			p.Eval(t, `document.querySelector('.site-header').style.visibility = 'hidden'`, nil)
			p.ShotElement(t, ".demo-stage", filepath.Join(dir, name+".png"), 1)
		}
	}
	click := func(label string) func(*browser.Page) {
		return func(p *browser.Page) {
			p.Eval(t, `[...document.querySelectorAll('.sx-choice button')].find(b => b.textContent === `+jsString(label)+`).click()`, nil)
		}
	}
	shot("stream", 1440, 900, "", nil, func(p *browser.Page) { p.Screenshot(t, filepath.Join(dir, "stream.png"), false) })
	shot("stream-stage", 1440, 900, "", nil, stage("stream-stage"))
	shot("stream-storm", 1440, 900, "", click("Storm"), stage("stream-storm"))
	shot("stream-light", 1440, 900, "lith-theme=light", click("Medium"), stage("stream-light"))
	shot("stream-smooth", 1440, 900, "lith-style=smooth", nil, stage("stream-smooth"))
	shot("stream-mobile", 390, 844, "", nil, func(p *browser.Page) { p.Screenshot(t, filepath.Join(dir, "stream-mobile.png"), true) })
	shot("stream-still", 1440, 900, "", click("Still"), stage("stream-still"))
	shot("stream-how", 1440, 900, "", func(p *browser.Page) {
		p.Eval(t, `document.querySelector('.howto').open = true`, nil)
	}, func(p *browser.Page) {
		p.Eval(t, `document.querySelector('.site-header').style.visibility = 'hidden'`, nil)
		p.ShotElement(t, ".howto", filepath.Join(dir, "stream-how.png"), 1)
	})

	// The landing page's slot, while signatures in the guestbook send its
	// stream a few frames.
	slot := func(name, cookie string) {
		c := client(t)
		get(t, c, base+"/")
		p := b.NewPage(t, 1440, 900)
		defer p.Close(t)
		p.Call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "reduce"}}})
		if cookie != "" {
			p.Navigate(t, base+"/")
			p.Eval(t, "document.cookie = '"+cookie+"; Path=/'", nil)
		}
		p.Navigate(t, base+"/")
		p.Eval(t, "document.fonts.ready.then(() => true)", nil)
		p.Wait(t, `/[0-9]/.test(document.querySelector('.sx-embed__nums div:last-child dd').textContent)`, 10*time.Second)
		for i := range 4 {
			post(t, c, base+"/cmd/guestbook/sign", `{"tab":"shots","op":"`+name+string(rune('a'+i))+`","name":"Ada","message":"Frame `+name+string(rune('a'+i))+`"}`)
			time.Sleep(1200 * time.Millisecond)
		}
		p.Eval(t, `document.querySelector('.site-header').style.visibility = 'hidden'`, nil)
		p.ShotElement(t, "#hypermedia", filepath.Join(dir, name+".png"), 1)
	}
	slot("stream-slot", "")
	slot("stream-slot-light", "lith-theme=light")

	card := b.NewPage(t, 1440, 900)
	card.Call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "reduce"}}})
	card.Navigate(t, base+"/demos")
	card.Eval(t, "document.fonts.ready.then(() => true)", nil)
	card.ShotElement(t, `[data-demo-card="stream"]`, filepath.Join(dir, "stream-card.png"), 1.5)
}

// jsString quotes s for JavaScript.
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestStreamLoad opens inspectors against a site that runs in a process of
// its own and reports what they cost it: CPU from /proc, the heap and the
// encoders' budget from its metrics, and the bytes the streams sent. The
// streams accept zstd and nothing decompresses them here.
//
//	go build -o /tmp/lith-site ./cmd/lith-site
//	GOMEMLIMIT=192MiB taskset -c 0-3 /tmp/lith-site -addr 127.0.0.1:8297 -db /tmp/load.db &
//	LITH_STREAM_LOAD=http://127.0.0.1:8297 LITH_STREAM_LOAD_PID=$! LITH_STREAM_LOAD_CHURN=busy \
//	    taskset -c 12-15 go test ./internal/app -run TestStreamLoad -v -timeout 10m
//
// LITH_STREAM_LOAD_N is the number of inspectors (100), _SIZE and _CHURN
// their payload (large, calm), _FOR how long it measures (60s), after 15 s
// to settle.
func TestStreamLoad(t *testing.T) {
	base := os.Getenv("LITH_STREAM_LOAD")
	if base == "" {
		t.Skip("set LITH_STREAM_LOAD to the URL of a running site")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	n, _ := strconv.Atoi(env("LITH_STREAM_LOAD_N", "100"))
	size, churn := env("LITH_STREAM_LOAD_SIZE", "large"), env("LITH_STREAM_LOAD_CHURN", "calm")
	window, err := time.ParseDuration(env("LITH_STREAM_LOAD_FOR", "60s"))
	if err != nil {
		t.Fatal(err)
	}
	pid := os.Getenv("LITH_STREAM_LOAD_PID")
	u, _ := url.Parse(base)
	port, _ := strconv.Atoi(u.Port())
	metricsURL := env("LITH_STREAM_LOAD_METRICS", "http://127.0.0.1:"+strconv.Itoa(port+100)+"/metrics")

	var wire atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := range n {
		c := client(t)
		get(t, c, base+"/demos/stream")
		tab := "load" + strconv.Itoa(i)
		req, _ := http.NewRequestWithContext(ctx, "GET", base+"/demos/stream/stream?tab="+tab, nil)
		req.Header.Set("Accept-Encoding", "zstd")
		res, err := c.Do(req)
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("stream %d: %v %v", i, err, res.Status)
		}
		go func() {
			defer res.Body.Close()
			buf := make([]byte, 32<<10)
			for {
				k, err := res.Body.Read(buf)
				wire.Add(int64(k))
				if err != nil {
					return
				}
			}
		}()
		body := `{"tab":"` + tab + `","op":"` + tab + `","size":"` + size + `","churn":"` + churn + `"}`
		if size == "large" && churn == "calm" {
			continue // a new tab's payload
		}
		for post(t, c, base+"/cmd/stream/payload", body) == http.StatusTooManyRequests {
			time.Sleep(200 * time.Millisecond)
		}
	}
	time.Sleep(15 * time.Second)

	cpu := func() float64 {
		if pid == "" {
			return 0
		}
		b, err := os.ReadFile("/proc/" + pid + "/stat")
		if err != nil {
			t.Fatal(err)
		}
		f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+2:]))
		user, _ := strconv.ParseFloat(f[11], 64)
		sys, _ := strconv.ParseFloat(f[12], 64)
		return (user + sys) / 100 // clock ticks of 10 ms
	}
	metric := func(name string) float64 {
		_, body := get(t, client(t), metricsURL)
		for line := range strings.Lines(body) {
			if strings.HasPrefix(line, name+" ") {
				v, _ := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, name+" ")), 64)
				return v
			}
		}
		return 0
	}
	c0, w0, f0, t0 := cpu(), wire.Load(), metric("lith_site_live_frames_sent_total"), time.Now()
	time.Sleep(window)
	c1, w1, f1, d := cpu(), wire.Load(), metric("lith_site_live_frames_sent_total"), time.Since(t0).Seconds()
	_, body := get(t, client(t), metricsURL)
	var tiers []string
	for line := range strings.Lines(body) {
		if strings.HasPrefix(line, "lith_site_sse_budget_streams{") {
			tiers = append(tiers, strings.TrimSpace(line))
		}
	}
	rss := ""
	if pid != "" {
		b, _ := os.ReadFile("/proc/" + pid + "/status")
		for line := range strings.Lines(string(b)) {
			if strings.HasPrefix(line, "VmRSS:") || strings.HasPrefix(line, "VmHWM:") {
				rss += strings.Join(strings.Fields(line), " ") + " "
			}
		}
	}
	t.Logf("%d inspectors, %s table, %s churn, over %.0f s:", n, size, churn, d)
	t.Logf("CPU %.2f cores; %s", (c1-c0)/d, rss)
	t.Logf("heap in use %.1f MiB, budget %.1f of %.1f MiB reserved", metric("go_memstats_heap_inuse_bytes")/(1<<20), metric("lith_site_sse_budget_reserved_bytes")/(1<<20), metric("lith_site_sse_budget_limit_bytes")/(1<<20))
	// Every stream gets the report view's probe once a second, a frame
	// that writes nothing while the table moves.
	frames := (f1-f0)/d - float64(n)
	t.Logf("%.0f frames a second; wire %.1f KB a second, %.0f bytes a frame", frames, float64(w1-w0)/d/1e3, float64(w1-w0)/d/frames)
	t.Logf("tiers: %s", strings.Join(tiers, "; "))
}
