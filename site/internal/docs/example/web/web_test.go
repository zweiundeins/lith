package web_test

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/edge"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/docs/example/board"
	"github.com/zweiundeins/lith/site/internal/docs/example/board/migrations"
	"github.com/zweiundeins/lith/site/internal/docs/example/web"
)

// TestBoardOnTheStream acts on board 1 and reads every outcome from the
// tab's stream: the lanes that changed, the op id that ends the pending
// state, a refusal, the panel, the session's preference.
func TestBoardOnTheStream(t *testing.T) {
	base, stop := start(t)
	c := client(t)

	page := get(t, c, base+"/boards/1")
	for _, want := range []string{`<h1>Launch</h1>`, `id="lane-1"`, `id="lane-2"`, `id="stream"`, `retry: &#39;error&#39;`} {
		if !strings.Contains(page, want) {
			t.Fatalf("no %s in the page:\n%s", want, page)
		}
	}

	events := listen(t, c, base+"/boards/1/stream?tab=tab1")
	next(t, events, `id="board"`)
	next(t, events, `1 tab on this board`)
	next(t, events, `"_ack":""`)

	if code := post(t, c, base+"/cmd/add", `{"tab":"tab1","op":"op1","lane":1,"title":"Write the docs"}`); code != http.StatusNoContent {
		t.Fatalf("add: %d", code)
	}
	ev := next(t, events, `Write the docs`)
	if strings.Contains(ev, `id="lane-2"`) || strings.Contains(ev, `id="board"`) {
		t.Errorf("the stream got more than the lane that changed:\n%s", ev)
	}
	next(t, events, `"_ack":"op1","_refused":false`)

	// The same op again (a retry): Once runs nothing, and nothing changes.
	if code := post(t, c, base+"/cmd/add", `{"tab":"tab1","op":"op1","lane":1,"title":"Write the docs"}`); code != http.StatusNoContent {
		t.Fatalf("retry: %d", code)
	}
	// A move with a version the card no longer has is refused, in this tab.
	if code := post(t, c, base+"/cmd/move", `{"tab":"tab1","op":"op2","card":1,"lane":2,"seen":7}`); code != http.StatusNoContent {
		t.Fatalf("move: %d", code)
	}
	next(t, events, `Someone moved this card meanwhile.`)
	next(t, events, `"_ack":"op2","_refused":true`)

	if code := post(t, c, base+"/cmd/panel", `{"tab":"tab1","card":1}`); code != http.StatusNoContent {
		t.Fatalf("panel: %d", code)
	}
	next(t, events, `<aside id="panel"><h2>Write the docs</h2>`)
	if code := post(t, c, base+"/cmd/compact", `{"compact":true}`); code != http.StatusNoContent {
		t.Fatalf("compact: %d", code)
	}
	next(t, events, `"_compact":true`)

	status := listen(t, c, base+"/status/stream")
	next(t, status, `<dl id="stats">`)
	next(t, status, `<dl id="stats">`) // a second later, without a change

	// A deploy: the hub stops, and the stream's last event re-creates the
	// element that opened it.
	stop()
	next(t, events, `document.getElementById(&#34;stream&#34;)`)
}

// TestCreateBoardOnce retries an action whose answer is an address: the
// retry gets the first board's, and no second board is made.
func TestCreateBoardOnce(t *testing.T) {
	base, _ := start(t)
	c := client(t)
	get(t, c, base+"/boards/1") // a session
	var answers []string
	for range 2 {
		req, _ := http.NewRequest("POST", base+"/cmd/boards", strings.NewReader(`{"op":"new1","name":"Second"}`))
		req.Header.Set("Datastar-Request", "true")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		answers = append(answers, string(body))
	}
	if !strings.Contains(answers[0], `"_goto":"/boards/2"`) || answers[1] != answers[0] {
		t.Errorf("answers %q", answers)
	}
}

// start serves the routes behind the part of the edge an action needs: a
// session, and the CSRF check. stop ends the hub, as a deploy does.
func start(t *testing.T) (base string, stop func()) {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), sqlite.Options{Migrations: migrations.FS})
	if err != nil {
		t.Fatal(err)
	}
	opts := web.HubOptions(db, slog.Default())
	opts.Workers, opts.Tick = 2, 10*time.Millisecond
	hub := live.New(opts)
	w, err := writer.New(db, writer.Options{OnCommit: hub.Changed})
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
	if _, err := board.Create(context.Background(), w, "Launch"); err != nil {
		t.Fatal(err)
	}

	h := http.Handler(web.New(db, w, hub, slog.Default(), "/static/datastar.js").Routes())
	h = edge.Sessions{}.Handler(h)
	h = edge.CSRF{Datastar: []string{"POST /cmd/"}}.Handler(h)
	srv := httptest.NewServer(h)
	stop = func() {
		stopHub()
		<-hubDone
	}
	t.Cleanup(func() {
		stop()
		srv.Close()
		w.Close()
		db.Close()
	})
	return srv.URL, stop
}

func client(t *testing.T) *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

func get(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	res, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d", url, res.StatusCode)
	}
	return string(body)
}

func post(t *testing.T, c *http.Client, url, body string) int {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Datastar-Request", "true")
	req.Header.Set("Sec-Fetch-Site", "same-origin") // as a browser sends it
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
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
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("the stream ended before %s", want)
			}
			if strings.Contains(ev, want) {
				return ev
			}
		case <-timeout:
			t.Fatalf("no event with %s", want)
		}
	}
}
