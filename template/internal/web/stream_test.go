package web_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/edge"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/template/internal/web"
	"github.com/zweiundeins/lith/template/migrations"
)

// TestSignOnTheStream posts the action, which answers 204, and reads its
// outcome from the tab's stream: the entry in the shared region, and the
// op id that ends the page's pending state.
func TestSignOnTheStream(t *testing.T) {
	base := start(t)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: jar}

	res, err := c.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(page), "<h1>Guestbook</h1>") {
		t.Fatalf("page: %d\n%s", res.StatusCode, page)
	}

	events := listen(t, c, base+"/stream?tab=tab1", nil)
	next(t, events, `<section id="entries"`)
	next(t, events, `"_ack":""`)

	if code := post(t, c, base+"/cmd/sign", `{"tab":"tab1","op":"op1","name":"Ada","message":"Hello"}`); code != http.StatusNoContent {
		t.Fatalf("sign: %d", code)
	}
	next(t, events, "<p>Hello</p>")
	next(t, events, `"_ack":"op1","_accepted":true`)

	if code := post(t, c, base+"/cmd/sign", `{"tab":"tab1","op":"op2","name":"Ada","message":"Hello"}`); code != http.StatusNoContent {
		t.Fatalf("sign again: %d", code)
	}
	next(t, events, "That is the newest entry already.")
	next(t, events, `"_ack":"op2","_accepted":false`)
}

// TestSlowLink checks the page's part of a slow link: it asks for
// Chromium's network hints, and its stream sends Save-Data: on at the same
// URL when the page measured a slow link or fell behind its stream; such a
// stream brings the same views.
func TestSlowLink(t *testing.T) {
	base := start(t)
	res, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if got := res.Header.Get("Accept-CH"); got != edge.NetworkHints {
		t.Errorf("Accept-CH: %q", got)
	}
	if !strings.Contains(string(page), "_slow: ((c, n) =&gt;") || !strings.Contains(string(page), "@get(&#39;/stream?tab=&#39; + $_tab, {headers: $_slow ? {&#39;Save-Data&#39;: &#39;on&#39;} : {}") {
		t.Errorf("the page does not measure its link:\n%s", page)
	}
	events := listen(t, http.DefaultClient, base+"/stream?tab=tab1", http.Header{"Save-Data": {"on"}})
	next(t, events, `<section id="entries"`)
	next(t, events, `"_ack":""`)
}

// TestNewestFrame checks that the page loads lith's newest-frame.js after
// Datastar, with an import map that maps 'datastar' to Datastar's URL, and
// turns it on on its body; the plugin goes out minified, under its
// content's hash, immutable.
func TestNewestFrame(t *testing.T) {
	base := start(t)
	res, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	page := string(b)
	m := regexp.MustCompile(`<script type="importmap" nonce="[^"]*">(.*?)</script>\s*<script type="module" src="(/static/vendor/datastar-rocket\.[0-9a-f]{12}\.js)"></script>\s*<script type="module" src="(/static/lith/newest-frame\.[0-9a-f]{12}\.js)"></script>`).FindStringSubmatch(page)
	if m == nil || !strings.Contains(page, "<body data-newest-frame>") {
		t.Fatalf("the page does not load newest-frame.js after Datastar, or does not turn it on:\n%s", page)
	}
	var importMap struct{ Imports map[string]string }
	if err := json.Unmarshal([]byte(m[1]), &importMap); err != nil || importMap.Imports["datastar"] != m[2] {
		t.Errorf("import map %s (%v), Datastar at %s", m[1], err, m[2])
	}
	res, err = http.Get(base + m[3])
	if err != nil {
		t.Fatal(err)
	}
	js, _ := io.ReadAll(res.Body)
	res.Body.Close()
	written, _ := fs.ReadFile(live.Scripts, live.NewestFrame)
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Cache-Control"), "immutable") || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/javascript") ||
		!regexp.MustCompile(`name:\s*["']newest-frame["']`).Match(js) || len(js) >= len(written) {
		t.Errorf("%s: %d, %s, %s, %d bytes of %d\n%.200s", m[3], res.StatusCode, res.Header.Get("Cache-Control"), res.Header.Get("Content-Type"), len(js), len(written), js)
	}
}

// start serves the routes behind the part of the edge an action needs: a
// session, and the CSRF check.
func start(t *testing.T) string {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), sqlite.Options{
		Synchronous: "NORMAL",
		Migrations:  migrations.FS,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := live.New(live.Options{Read: db.R, Workers: 2})
	w, err := writer.New(db, writer.Options{OnCommit: hub.Changed})
	if err != nil {
		t.Fatal(err)
	}
	site, err := web.New(db, w, hub, slog.Default(), false)
	if err != nil {
		t.Fatal(err)
	}
	hubCtx, stopHub := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() {
		hub.Run(hubCtx)
		close(hubDone)
	}()
	go w.Run(context.Background())

	h := http.Handler(site.Routes())
	h = edge.Sessions{}.Handler(h)
	h = edge.CSRF{Datastar: []string{"POST /cmd/"}}.Handler(h)
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		stopHub()
		<-hubDone
		srv.Close()
		w.Close()
		db.Close()
	})
	return srv.URL
}

// listen opens a stream, with header on its request, and sends each of its
// events to the channel.
func listen(t *testing.T, c *http.Client, url string, header http.Header) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Accept-Encoding", "identity")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream: %d", res.StatusCode)
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

// next skips events until one contains want.
func next(t *testing.T, events <-chan string, want string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("the stream ended before an event with %q", want)
			}
			if strings.Contains(event, want) {
				return
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
