package app_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/app"
	"github.com/zweiundeins/lith/site/internal/app/apptest"
)

func client(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func get(t *testing.T, c *http.Client, url string) (*http.Response, string) {
	t.Helper()
	res, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, string(b)
}

func TestPages(t *testing.T) {
	_, base := apptest.Start(t)
	c := client(t)
	for path, want := range map[string]string{
		"/":                     `aria-label="lith: Linear writes, Immediate mode, Ticks, Hypermedia"`,
		"/docs":                 `<h1 class="page-title">Documentation</h1>`,
		"/docs/getting-started": `<h1 class="page-title">Getting started</h1>`,
		"/docs/concepts/ticks":  `<h1 class="page-title">Ticks</h1>`,
		"/docs/credits":         `hyperlith`,
		"/demos":                `data-demo-card="guestbook"`,
		"/demos/guestbook":      `data-demo="guestbook"`,
	} {
		res, body := get(t, c, base+path)
		if res.StatusCode != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s: %d, no %s", path, res.StatusCode, want)
			continue
		}
		csp := res.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "'nonce-") || !strings.Contains(csp, "'unsafe-eval'") {
			t.Errorf("%s: CSP %q", path, csp)
		}
		if !strings.Contains(body, `<link rel="canonical" href="`+base+path+`"`) {
			t.Errorf("%s: no canonical link", path)
		}
	}
	for _, path := range []string{"/nope", "/docs/nope", "/demos/nope"} {
		if res, body := get(t, c, base+path); res.StatusCode != http.StatusNotFound || !strings.Contains(body, "Nothing here") {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
	if res, _ := get(t, c, base+"/docs/concepts/"); res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != "/docs/concepts" {
		t.Errorf("/docs/concepts/: %d to %q", res.StatusCode, res.Header.Get("Location"))
	}
	// The theme and the corners come from their cookies.
	req, _ := http.NewRequest("GET", base+"/docs", nil)
	req.AddCookie(&http.Cookie{Name: "lith-theme", Value: "light"})
	req.AddCookie(&http.Cookie{Name: "lith-style", Value: "smooth"})
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(b), `data-lith-theme="light" data-lith-style="smooth"`) || !strings.Contains(string(b), `<meta name="color-scheme" content="light">`) {
		t.Error("the preferences' cookies are not rendered")
	}
}

var assetURL = regexp.MustCompile(`(?:href|src)="(/static/[^"]+)"`)

func TestStatic(t *testing.T) {
	_, base := apptest.Start(t)
	c := client(t)
	_, page := get(t, c, base+"/")
	urls := assetURL.FindAllStringSubmatch(page, -1)
	if len(urls) < 4 {
		t.Fatalf("assets: %v", urls)
	}
	for _, m := range urls {
		res, body := get(t, c, base+m[1])
		if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
			t.Errorf("%s: %d %q", m[1], res.StatusCode, res.Header.Get("Cache-Control"))
		}
		if strings.Contains(m[1], "site.") && strings.HasSuffix(m[1], ".css") {
			// Minified: no comments, no spaces it can do without.
			if !strings.HasPrefix(body, "@layer reset,tokens,theme,base,layout,components,utilities;") || strings.Contains(body, "/*") {
				t.Errorf("the bundle does not start with the layers' order, or is not minified")
			}
			if strings.Contains(body, "../fonts/") || !regexp.MustCompile(`url\("?/static/fonts/pixelify-sans\.[0-9a-f]+\.woff2"?\)`).MatchString(body) {
				t.Error("the bundle's font URLs are not hashed")
			}
			if !strings.Contains(body, `@scope([data-demo="guestbook"])`) {
				t.Error("the guestbook's stylesheet is not in the bundle")
			}
		}
	}
	for path, want := range map[string]string{
		"/favicon.ico":          "image/png",
		"/apple-touch-icon.png": "image/png",
		"/og.png":               "image/png",
		"/robots.txt":           "text/plain",
		"/sitemap.xml":          "application/xml",
	} {
		if res, _ := get(t, c, base+path); res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), want) {
			t.Errorf("%s: %d %s", path, res.StatusCode, res.Header.Get("Content-Type"))
		}
	}
	_, sitemap := get(t, c, base+"/sitemap.xml")
	if !strings.Contains(sitemap, base+"/docs/concepts/ticks</loc>") || !strings.Contains(sitemap, base+"/demos/guestbook</loc>") || !strings.Contains(sitemap, base+"/docs/reference/writer</loc>") {
		t.Errorf("sitemap:\n%s", sitemap)
	}
}

func TestProbes(t *testing.T) {
	_, base := apptest.Start(t)
	c := client(t)
	if res, body := get(t, c, base+"/healthz"); res.StatusCode != http.StatusOK || strings.TrimSpace(body) != "ok test" {
		t.Errorf("healthz: %d %q", res.StatusCode, body)
	}
	if res, body := get(t, c, base+"/readyz"); res.StatusCode != http.StatusOK {
		t.Errorf("readyz: %d %q", res.StatusCode, body)
	}
}

// TestMetrics reads the metrics from their own handler, and finds none on
// the public one.
func TestMetrics(t *testing.T) {
	a, base := apptest.Start(t)
	c := client(t)
	get(t, c, base+"/")
	get(t, c, base+"/demos/guestbook")
	if res, body := get(t, c, base+"/metrics"); res.StatusCode != http.StatusNotFound || strings.Contains(body, "go_goroutines") {
		t.Errorf("/metrics on the public listener: %d", res.StatusCode)
	}
	rec := httptest.NewRecorder()
	a.Metrics().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`lith_site_http_requests_total{code="200",route="GET /{$}"} 1`,
		`lith_site_http_requests_total{code="200",route="GET /demos/{slug}"} 1`,
		`lith_site_http_request_duration_seconds_bucket{route="GET /{$}",le="0.1"}`,
		`lith_site_http_requests_in_flight`,
		`lith_site_writer_commands_total`,
		`lith_site_writer_commit_p99_seconds`,
		`lith_site_live_ticks_total`,
		`lith_site_live_streams`,
		`lith_site_live_frames_dropped_total`,
		`lith_site_sse_budget_limit_bytes`,
		`lith_site_sse_budget_refused_total`,
		`lith_site_db_query_duration_seconds_bucket{op="query"`,
		`lith_site_build_info{version="test"} 1`,
		`go_goroutines`,
		`go_memstats_heap_inuse_bytes`,
		`process_cpu_seconds_total`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no %s", want)
		}
	}
}

// TestGuestbook posts a signature, which answers 204, and reads what
// became of it from the tab's stream: the entry in the shared list (inside
// a view transition, after the stream's first frame), and the op id that
// ends the page's pending state. Then the same words again, refused.
func TestGuestbook(t *testing.T) {
	_, base := apptest.Start(t)
	c := client(t)
	get(t, c, base+"/demos/guestbook") // the session's cookie

	events := listen(t, c, base+"/demos/guestbook/stream?tab=tab1")
	first := next(t, events, `id="guestbook-entries"`)
	if strings.Contains(first, "useViewTransition") {
		t.Errorf("the first frame of the list animates:\n%s", first)
	}
	next(t, events, `"ack":""`)

	if code := post(t, c, base+"/cmd/guestbook/sign", `{"tab":"tab1","op":"op1","name":"Ada","message":"Hello from the test"}`); code != http.StatusNoContent {
		t.Fatalf("sign: %d", code)
	}
	list := next(t, events, "Hello from the test")
	if !strings.Contains(list, "useViewTransition true") {
		t.Errorf("a later frame of the list does not animate:\n%s", list)
	}
	next(t, events, `"ack":"op1","accepted":true`)

	if code := post(t, c, base+"/cmd/guestbook/sign", `{"tab":"tab1","op":"op2","name":"Ada","message":"Hello from the test"}`); code != http.StatusNoContent {
		t.Fatalf("sign again: %d", code)
	}
	next(t, events, "That is the newest entry already.")
	next(t, events, `"ack":"op2","accepted":false`)

	// The landing page's stream carries the guestbook's slot.
	home := listen(t, c, base+"/stream?tab=tab2")
	next(t, home, "Hello from the test")
}

// TestActions checks the edge in front of the actions: no Datastar
// header, no action; a burst beyond the rate, 429; a bad tab id, 400.
func TestActions(t *testing.T) {
	_, base := apptest.Start(t)
	c := client(t)
	get(t, c, base+"/")
	req, _ := http.NewRequest("POST", base+"/cmd/guestbook/sign", strings.NewReader(`{"tab":"t","op":"o","name":"A","message":"B"}`))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if res, err := c.Do(req); err != nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("a cross-site action: %v %v", res.StatusCode, err)
	}
	if code := post(t, c, base+"/cmd/guestbook/sign", `{"tab":"bad id!","op":"o1","name":"A","message":"B"}`); code != http.StatusBadRequest {
		t.Errorf("a bad tab id: %d", code)
	}
	listen(t, c, base+"/demos/guestbook/stream?tab=t1")
	codes := []int{}
	for i := range 4 {
		codes = append(codes, post(t, c, base+"/cmd/guestbook/sign", `{"tab":"t1","op":"op`+string(rune('a'+i))+`","name":"A","message":"Message `+string(rune('a'+i))+`"}`))
	}
	// The bad request above took the first token of the burst of three.
	if codes[0] != 204 || codes[1] != 204 || codes[3] != http.StatusTooManyRequests {
		t.Errorf("a burst of signatures: %v", codes)
	}
}

// TestActionNeedsAStream refuses an action for a tab whose stream the
// session has not open: its state would be a row for a tab id the client
// made up. Another session's stream of that tab does not count.
func TestActionNeedsAStream(t *testing.T) {
	_, base := apptest.Start(t)
	ada, bob := client(t), client(t)
	get(t, ada, base+"/demos/guestbook")
	get(t, bob, base+"/demos/guestbook")
	sign := `{"tab":"t1","op":"o1","name":"A","message":"B"}`
	if code := post(t, ada, base+"/cmd/guestbook/sign", sign); code != http.StatusBadRequest {
		t.Errorf("a tab without a stream: %d", code)
	}
	listen(t, bob, base+"/demos/guestbook/stream?tab=t1")
	if code := post(t, ada, base+"/cmd/guestbook/sign", sign); code != http.StatusBadRequest {
		t.Errorf("another session's tab: %d", code)
	}
	listen(t, ada, base+"/demos/guestbook/stream?tab=t1")
	if code := post(t, ada, base+"/cmd/guestbook/sign", sign); code != http.StatusNoContent {
		t.Errorf("the session's own tab: %d", code)
	}
}

// TestPublicWithoutCookie: the answers that are the same for everyone,
// the static files, the icons, robots.txt and the sitemap, carry no
// session cookie that a shared cache would hand to everyone, while a page
// mints one.
func TestPublicWithoutCookie(t *testing.T) {
	_, base := apptest.Start(t)
	res, page := get(t, http.DefaultClient, base+"/")
	if res.Header.Get("Set-Cookie") == "" {
		t.Fatal("the page minted no session")
	}
	css := regexp.MustCompile(`/static/[a-z/]+\.[0-9a-f]{12}\.css`).FindString(page)
	if css == "" {
		t.Fatal("no stylesheet on the page")
	}
	for _, path := range []string{css, "/favicon.ico", "/og.png", "/robots.txt", "/sitemap.xml"} {
		res, _ := get(t, http.DefaultClient, base+path)
		if res.StatusCode != http.StatusOK || len(res.Header.Values("Set-Cookie")) > 0 || res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: %d, Set-Cookie %q, X-Content-Type-Options %q", path, res.StatusCode, res.Header.Values("Set-Cookie"), res.Header.Get("X-Content-Type-Options"))
		}
	}
}

// TestStreamsPerClient: one client address holds -client-streams streams
// at most and gets 429 for more, while another address still gets one.
// The address comes from X-Forwarded-For, which the site believes from
// loopback, as from Caddy.
func TestStreamsPerClient(t *testing.T) {
	_, base := apptest.Start(t, func(c *app.Config) { c.MaxStreams, c.ClientStreams = 5, 2 })
	open := func(addr string, tab int) int {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		req, _ := http.NewRequestWithContext(ctx, "GET", base+"/demos/guestbook/stream?tab=tab"+strconv.Itoa(tab), nil)
		req.Header.Set("X-Forwarded-For", addr)
		req.Header.Set("Accept-Encoding", "identity")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusOK {
			res.Body.Close()
		}
		return res.StatusCode
	}
	for i, want := range []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests} {
		if got := open("198.51.100.7", i); got != want {
			t.Fatalf("the client's stream %d: %d, want %d", i+1, got, want)
		}
	}
	if got := open("203.0.113.9", 9); got != http.StatusOK {
		t.Fatalf("another client's stream: %d", got)
	}
}

// TestReconnect stops the streams as a deploy does: each one's last event
// re-creates the element that opened it.
func TestReconnect(t *testing.T) {
	a, base := apptest.Start(t)
	c := client(t)
	events := listen(t, c, base+"/demos/guestbook/stream?tab=tab1")
	next(t, events, `id="guestbook-entries"`)
	a.StopStreams()
	last := next(t, events, `getElementById(&#34;stream&#34;)`)
	if !strings.Contains(last, "datastar-patch-elements") || !strings.Contains(last, "cloneNode") {
		t.Errorf("the last event:\n%s", last)
	}
}

func TestSweep(t *testing.T) {
	a, _ := apptest.Start(t)
	if err := a.SweepNow(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
}

// listen opens a stream and sends each of its events to the channel.
func listen(t *testing.T, c *http.Client, url string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "identity")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream %s: %d", url, res.StatusCode)
	}
	events := make(chan string)
	go func() {
		defer res.Body.Close()
		defer close(events)
		lines := bufio.NewScanner(res.Body)
		lines.Buffer(nil, 1<<20)
		var event strings.Builder
		for lines.Scan() {
			if lines.Text() != "" {
				event.WriteString(lines.Text() + "\n")
				continue
			}
			if event.Len() > 0 {
				select {
				case events <- event.String():
				case <-ctx.Done():
					return
				}
				event.Reset()
			}
		}
	}()
	return events
}

// next skips events until one contains want, and returns it.
func next(t *testing.T, events <-chan string, want string) string {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("the stream ended before an event with %q", want)
			}
			if strings.Contains(event, want) {
				return event
			}
		case <-timeout:
			t.Fatalf("no event with %q in 5s", want)
		}
	}
}

// post sends an action the way Datastar does.
func post(t *testing.T, c *http.Client, url, body string) int {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Datastar-Request", "true")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res.StatusCode
}
