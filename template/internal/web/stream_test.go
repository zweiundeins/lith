package web_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
	next(t, events, `"sign":{"ack":"","refused":false}`)

	if code := post(t, c, base+"/cmd/sign", `{"tab":"tab1","op":"op1","name":"Ada","message":"Hello"}`); code != http.StatusNoContent {
		t.Fatalf("sign: %d", code)
	}
	next(t, events, "<p>Hello</p>")
	next(t, events, `"sign":{"ack":"op1","refused":false}`)

	if code := post(t, c, base+"/cmd/sign", `{"tab":"tab1","op":"op2","name":"Ada","message":"Hello"}`); code != http.StatusNoContent {
		t.Fatalf("sign again: %d", code)
	}
	next(t, events, "That is the newest entry already.")
	next(t, events, `"sign":{"ack":"op2","refused":true}`)
}

// TestNotices reads on the tab's stream what no command decided: a
// signature over the address's limit gets a bare 429, and the tab a notice
// with its op id, which ends the page's pending state; a deploy ends the
// stream with a notice, then the event that opens it again.
func TestNotices(t *testing.T) {
	base, _, stop := startStoppable(t)
	c := client(t, base)
	events := listen(t, c, base+"/stream?tab=tab1", nil)
	next(t, events, `"sign":{"ack":"","refused":false}`)
	limited := ""
	for i := range 20 {
		op := "op" + strconv.Itoa(i)
		res, err := c.Do(action(t, base+"/cmd/sign", `{"tab":"tab1","op":"`+op+`","name":"Ada","message":"Hello `+op+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode == http.StatusTooManyRequests {
			if len(body) != 0 {
				t.Errorf("a 429 with a body: %q", body)
			}
			limited = op
			break
		}
		if res.StatusCode != http.StatusNoContent {
			t.Fatalf("sign %s: %d", op, res.StatusCode)
		}
	}
	if limited == "" {
		t.Fatal("no signature reached the limit")
	}
	next(t, events, `<p id="outcome" role="status" class="refused">Too many signatures at once.`)
	next(t, events, `"sign":{"ack":"`+limited+`","refused":true}`)

	// Over the limit, the tab and the op in the headers are enough: the
	// notice goes out before the body is read, so a body that does not
	// parse still gets a 429 and its notice.
	req := action(t, base+"/cmd/sign", `{"tab":"tab1","op":"over"}`)
	req.Body = io.NopCloser(strings.NewReader("not JSON"))
	req.ContentLength = -1
	if code := do(t, c, req); code != http.StatusTooManyRequests {
		t.Fatalf("a request over the limit: %d, want 429", code)
	}
	next(t, events, `"sign":{"ack":"over","refused":true}`)

	stop()
	next(t, events, `The server is updating. Back in a moment.`)
	next(t, events, `document.getElementById(&#34;stream&#34;)`)
}

// TestRefresh opens streams as pages that a former build rendered do after
// a deploy: an untouched page gets #content first, then the views' frames,
// then $_build; a page with typed input or other assets gets the notice
// instead; a page of the running build gets its frames alone.
func TestRefresh(t *testing.T) {
	base, _, _ := startBuild(t, "new")
	c := client(t, base)
	res, err := c.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	m := regexp.MustCompile(`_build: &#34;new&#34;, _assets: &#34;([0-9a-f]+)&#34;`).FindSubmatch(page)
	if m == nil {
		t.Fatalf("the page lacks its build and assets:\n%s", page)
	}
	assets := string(m[1])
	if !strings.Contains(string(page), `filterSignals: {include: /^_(build|assets|dirty)$/, exclude: /(?!)/}`) {
		t.Fatalf("the stream does not report the page's build, assets and typing:\n%s", page)
	}
	open := func(tab, build, assets string, dirty bool) <-chan string {
		signals, err := json.Marshal(map[string]any{"_build": build, "_assets": assets, "_dirty": dirty})
		if err != nil {
			t.Fatal(err)
		}
		return listen(t, c, base+"/stream?tab="+tab+"&datastar="+url.QueryEscape(string(signals)), http.Header{"Page-Path": {"/"}})
	}

	events := open("tab1", "old", assets, false)
	if first := <-events; !strings.Contains(first, `<div id="content">`) || strings.Contains(first, `id="stream"`) {
		t.Fatalf("the first event is %q", first)
	}
	next(t, events, `<section id="entries"`)
	next(t, events, `"sign":{"ack":"","refused":false}`)
	next(t, events, `{"_build":"new"}`)

	events = open("tab2", "old", assets, true)
	if first := <-events; !strings.Contains(first, `<p id="update" role="status">This page has a new version. Reloading clears the form.`) {
		t.Fatalf("a page with typed input got %q", first)
	}
	next(t, events, `{"_build":"new"}`)

	events = open("tab3", "old", "stale", false)
	if first := <-events; !strings.Contains(first, `<p id="update" role="status">This page has a new version. <button`) {
		t.Fatalf("a page with other assets got %q", first)
	}

	events = open("tab4", "new", assets, false)
	if first := <-events; !strings.Contains(first, `<section id="entries"`) {
		t.Fatalf("a page of the running build got %q", first)
	}
	none(t, events, `_build`, 100*time.Millisecond)
}

// TestSignNeedsAStream refuses a signature from a tab whose stream is not
// open: its outcome would reach no page, and its row would come from a tab
// id the client made up. Another session's stream with that tab id does not
// count.
func TestSignNeedsAStream(t *testing.T) {
	base := start(t)
	ada, bob := client(t, base), client(t, base)
	if code := post(t, ada, base+"/cmd/sign", `{"tab":"made-up","op":"op1","name":"Ada","message":"Hello"}`); code != http.StatusConflict {
		t.Fatalf("a tab without a stream: %d, want 409", code)
	}
	events := listen(t, ada, base+"/stream?tab=tab1", nil)
	next(t, events, `"sign":{"ack":"","refused":false}`)
	if code := post(t, bob, base+"/cmd/sign", `{"tab":"tab1","op":"op1","name":"Bob","message":"Hello"}`); code != http.StatusConflict {
		t.Fatalf("another session's tab: %d, want 409", code)
	}
	if code := post(t, ada, base+"/cmd/sign", `{"tab":"tab1","op":"op1","name":"Ada","message":"Hello"}`); code != http.StatusNoContent {
		t.Fatalf("the session's own tab: %d", code)
	}
	next(t, events, "<p>Hello</p>")
}

// TestSlowLink checks the page's part of a slow link: it asks for
// Chromium's network hints, and its stream sends Save-Data: on at the same
// URL when the page measured a slow link; such a stream brings the same
// views.
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
	if !strings.Contains(string(page), "_slow: ((c, n) =&gt;") || !strings.Contains(string(page), "@get(&#39;/stream?tab=&#39; + $_tab, {headers: {") || !strings.Contains(string(page), "...($_slow ? {&#39;Save-Data&#39;: &#39;on&#39;} : {})}") {
		t.Errorf("the page does not measure its link:\n%s", page)
	}
	events := listen(t, http.DefaultClient, base+"/stream?tab=tab1", http.Header{"Save-Data": {"on"}})
	next(t, events, `<section id="entries"`)
	next(t, events, `"sign":{"ack":"","refused":false}`)
}

// TestNewestFrame checks that the page loads lith's newest-frame.js after
// Datastar, with an import map that maps 'datastar' to Datastar's URL, and
// turns it on on its body, which posts /pace when the plugin reports
// that the page fell behind; the plugin, a minified build, goes out as
// vendored, under its content's hash, immutable.
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
	if m == nil || !strings.Contains(page, `<body data-newest-frame data-on:newest-frame-behind="@post(&#39;/pace?tab=&#39; + $_tab, {requestCancellation: &#39;disabled&#39;})">`) {
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
		!regexp.MustCompile(`name:\s*["']newest-frame["']`).Match(js) || string(js) != string(written) {
		t.Errorf("%s: %d, %s, %s, %d bytes of %d\n%.200s", m[3], res.StatusCode, res.Header.Get("Cache-Control"), res.Header.Get("Content-Type"), len(js), len(written), js)
	}
}

// TestPacing paces a tab's stream as the page does once it falls behind:
// /pace counts the report and paces the stream, whose next write ends with
// an element that posts /ack for it after the paint. The stream then holds
// its next frame until /ack, with the stream's token, the write's number
// and the page's round trip, frees it.
func TestPacing(t *testing.T) {
	base, hub := startHub(t)
	c := client(t, base)
	events := listen(t, c, base+"/stream?tab=tab1", nil)
	next(t, events, `"sign":{"ack":"","refused":false}`)
	if code := post(t, c, base+"/pace?tab=tab1", ""); code != http.StatusNoContent {
		t.Fatalf("pace: %d", code)
	}
	if st := hub.Stats(); st.Paces != 1 || st.Behind != 1 || st.BehindStreams != 1 {
		t.Fatalf("Paces %d, Behind %d, BehindStreams %d, want 1, 1 and 1", st.Paces, st.Behind, st.BehindStreams)
	}
	token := sign(t, c, events, base, "op1", "Hello")
	if code := post(t, c, base+"/cmd/sign", `{"tab":"tab1","op":"op2","name":"Ada","message":"Again"}`); code != http.StatusNoContent {
		t.Fatalf("sign again: %d", code)
	}
	none(t, events, "<p>Again</p>", 300*time.Millisecond)
	if code := post(t, c, base+"/ack?tab=tab1&s="+token+"&n=1&rtt=12", ""); code != http.StatusNoContent {
		t.Fatalf("ack: %d", code)
	}
	next(t, events, "<p>Again</p>")
	next(t, events, `id="ack-2"`)
	if st := hub.Stats(); st.Acks != 1 {
		t.Errorf("Acks %d, want 1", st.Acks)
	}
}

// TestPacingIsTheSessionsOwn posts /pace and /ack for a tab's stream from
// another session: both answer 204 and reach nothing, since the stream is
// named after its session.
func TestPacingIsTheSessionsOwn(t *testing.T) {
	base, hub := startHub(t)
	ada, bob := client(t, base), client(t, base)
	events := listen(t, ada, base+"/stream?tab=tab1", nil)
	next(t, events, `"sign":{"ack":"","refused":false}`)
	if code := post(t, bob, base+"/pace?tab=tab1", ""); code != http.StatusNoContent {
		t.Fatalf("pace from another session: %d", code)
	}
	if st := hub.Stats(); st.Paces != 0 || st.Behind != 0 {
		t.Fatalf("another session's /pace: Paces %d, Behind %d", st.Paces, st.Behind)
	}
	post(t, ada, base+"/pace?tab=tab1", "")
	token := sign(t, ada, events, base, "op1", "Hello")
	ackURL := base + "/ack?tab=tab1&s=" + token + "&n=1&rtt=12"
	if code := post(t, bob, ackURL, ""); code != http.StatusNoContent {
		t.Fatalf("ack from another session: %d", code)
	}
	if st := hub.Stats(); st.Acks != 0 {
		t.Fatalf("another session's /ack was taken: Acks %d", st.Acks)
	}
	post(t, ada, ackURL, "")
	if st := hub.Stats(); st.Acks != 1 {
		t.Errorf("the session's own /ack: Acks %d, want 1", st.Acks)
	}
}

// TestPacingLimits checks the limits: five reports of a session at once,
// then one a second, and 40 acknowledgements of a stream at once, then 20 a
// second, which another stream of the same session does not share. Over the
// limit a post answers 204 and is dropped.
func TestPacingLimits(t *testing.T) {
	base, hub := startHub(t)
	c := client(t, base)
	for i := range 7 {
		tab := "tab" + strconv.Itoa(i)
		events := listen(t, c, base+"/stream?tab="+tab, nil)
		next(t, events, `"sign":{"ack":"","refused":false}`)
		if code := post(t, c, base+"/pace?tab="+tab, ""); code != http.StatusNoContent {
			t.Fatalf("pace %s: %d", tab, code)
		}
	}
	if st := hub.Stats(); st.Paces != 5 || st.Behind != 5 {
		t.Errorf("seven reports at once: Paces %d, Behind %d, want 5 and 5", st.Paces, st.Behind)
	}

	c = client(t, base)
	events := listen(t, c, base+"/stream?tab=tab1", nil)
	next(t, events, `"sign":{"ack":"","refused":false}`)
	post(t, c, base+"/pace?tab=tab1", "")
	token := sign(t, c, events, base, "op1", "Hello")
	for range 80 {
		if code := post(t, c, base+"/ack?tab=tab1&s="+token+"&n=1", ""); code != http.StatusNoContent {
			t.Fatalf("ack: %d", code)
		}
	}
	taken := hub.Stats().Acks
	if taken < 40 || taken > 44 {
		t.Errorf("80 acknowledgements at once: %d taken, want 40 and what refilled", taken)
	}

	other := listen(t, c, base+"/stream?tab=tab2", nil)
	next(t, other, `"sign":{"ack":"","refused":false}`)
	post(t, c, base+"/pace?tab=tab2", "")
	if code := post(t, c, base+"/cmd/sign", `{"tab":"tab2","op":"op2","name":"Ada","message":"Hello again"}`); code != http.StatusNoContent {
		t.Fatalf("sign: %d", code)
	}
	m := regexp.MustCompile(`/ack\?tab=tab2&amp;s=([0-9a-z]+)&amp;n=1&amp;rtt=`).FindStringSubmatch(next(t, other, `id="ack-1"`))
	if m == nil {
		t.Fatal("no acknowledgement of the other stream's write")
	}
	token2 := m[1]
	for range 5 {
		post(t, c, base+"/ack?tab=tab2&s="+token2+"&n=1", "")
	}
	if n := hub.Stats().Acks - taken; n < 5 {
		t.Errorf("the same session's other stream: %d of 5 acknowledgements taken, want 5", n)
	}
}

// TestPacingInput refuses what no page sends, and the edge refuses a post
// without Datastar's header.
func TestPacingInput(t *testing.T) {
	base := start(t)
	c := client(t, base)
	for _, url := range []string{
		"/pace?tab=no%20tab", "/pace",
		"/ack?tab=tab1&n=1", "/ack?tab=tab1&s=X&n=1", "/ack?tab=tab1&s=1&n=0", "/ack?tab=tab1&s=1&n=x",
		"/ack?tab=tab1&s=1&n=1&rtt=x", "/ack?s=1&n=1",
	} {
		if code := post(t, c, base+url, ""); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", url, code)
		}
	}
	for _, url := range []string{"/pace?tab=tab1", "/ack?tab=tab1&s=1&n=1"} {
		req, _ := http.NewRequest("POST", base+url, nil)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		if res, err := c.Do(req); err != nil || res.StatusCode != http.StatusForbidden {
			t.Errorf("%s without Datastar-Request: %v, %v", url, res, err)
		} else {
			res.Body.Close()
		}
	}
}

// sign signs the guestbook from tab1 of a paced stream, and returns the
// stream's token from the element that acknowledges the write.
func sign(t *testing.T, c *http.Client, events <-chan string, base, op, message string) string {
	t.Helper()
	if code := post(t, c, base+"/cmd/sign", `{"tab":"tab1","op":"`+op+`","name":"Ada","message":"`+message+`"}`); code != http.StatusNoContent {
		t.Fatalf("sign: %d", code)
	}
	ack := `/ack\?tab=tab1&amp;s=([0-9a-z]+)&amp;n=1&amp;rtt=`
	m := regexp.MustCompile(ack).FindStringSubmatch(next(t, events, `id="ack-1"`))
	if m == nil {
		t.Fatalf("no acknowledgement of the write matches %s", ack)
	}
	return m[1]
}

// start serves the routes behind the part of the edge an action needs: a
// session, and the CSRF check.
func start(t *testing.T) string {
	t.Helper()
	base, _ := startHub(t)
	return base
}

// startHub is start, and returns the hub as well.
func startHub(t *testing.T) (string, *live.Hub) {
	t.Helper()
	base, hub, _ := startStoppable(t)
	return base, hub
}

// startStoppable is startHub, and returns a func that stops the hub, as a
// deploy does.
func startStoppable(t *testing.T) (string, *live.Hub, func()) {
	t.Helper()
	return startBuild(t, "")
}

// startBuild is startStoppable with a hub that runs the build.
func startBuild(t *testing.T, build string) (string, *live.Hub, func()) {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), sqlite.Options{
		Synchronous: "NORMAL",
		Migrations:  migrations.FS,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := live.New(live.Options{Read: db.R, Workers: 2, Stopping: web.Stopping, Build: build})
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
	h = edge.CSRF{Datastar: []string{"POST /cmd/", "POST /pace", "POST /ack"}}.Handler(h)
	mux := http.NewServeMux()
	mux.Handle("GET /static/", site.Static()) // outside the sessions, as cmd/app has it
	mux.Handle("/", h)
	srv := httptest.NewServer(mux)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			stopHub()
			<-hubDone
		})
	}
	t.Cleanup(func() {
		stop()
		srv.Close()
		w.Close()
		db.Close()
	})
	return srv.URL, hub, stop
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

// none fails if an event with unwanted comes within d.
func none(t *testing.T, events <-chan string, unwanted string, d time.Duration) {
	t.Helper()
	timeout := time.After(d)
	for {
		select {
		case event := <-events:
			if strings.Contains(event, unwanted) {
				t.Fatalf("an event with %q came while the stream waited", unwanted)
			}
		case <-timeout:
			return
		}
	}
}

// client is a browser's client of the routes, with a session of its own.
func client(t *testing.T, base string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: jar}
	res, err := c.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return c
}

// post sends an action the way the page does (action).
func post(t *testing.T, c *http.Client, url, body string) int {
	t.Helper()
	return do(t, c, action(t, url, body))
}

// action is an op's request the way the page sends it (views.act): body's
// "tab" and "op" go in the Tab-Id and Op-Id headers, the rest in the body.
func action(t *testing.T, url, body string) *http.Request {
	t.Helper()
	var tab, op string
	payload := []byte(body)
	if body != "" {
		var fields map[string]any
		if err := json.Unmarshal(payload, &fields); err != nil {
			t.Fatal(err)
		}
		tab, _ = fields["tab"].(string)
		op, _ = fields["op"].(string)
		delete(fields, "tab")
		delete(fields, "op")
		var err error
		if payload, err = json.Marshal(fields); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Datastar-Request", "true")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if tab != "" {
		req.Header.Set("Tab-Id", tab)
	}
	if op != "" {
		req.Header.Set("Op-Id", op)
	}
	return req
}

// do sends req and returns its status.
func do(t *testing.T, c *http.Client, req *http.Request) int {
	t.Helper()
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res.StatusCode
}
