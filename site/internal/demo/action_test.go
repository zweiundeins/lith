package demo_test

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
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
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// An action over its limits, and one whose write fails, answer with a
// bare status, and the tab's stream gets a notice in the demo's signals
// with the op id it answers. A tab whose stream the session does not hold
// gets no notice.
func TestActionNotices(t *testing.T) {
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "kit.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	w, err := writer.New(db, writer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	go w.Run(context.Background())
	hub := live.New(live.Options{Read: db.R, Log: slog.New(slog.DiscardHandler)})
	ctx, stop := context.WithCancel(context.Background())
	go hub.Run(ctx)
	t.Cleanup(func() {
		stop()
		w.Close()
		db.Close()
	})

	mux := http.NewServeMux()
	k := demo.NewKit("cursor-party", mux, demo.Deps{DB: db, W: w, Hub: hub, Log: slog.New(slog.DiscardHandler),
		Open: func(v demo.Viewer) bool { return v.Tab == "t1" }})
	k.Action("go", demo.Rate{Every: time.Hour, Burst: 1}, func(rw http.ResponseWriter, r *http.Request) {
		var req struct{ Tab, Op string }
		if !demo.Decode(rw, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		k.Act(rw, r, v, demo.Ack{"ack": req.Op}, writer.Func(func(*writer.Tx) error { return errors.New("disk full") }))
	})
	mux.HandleFunc("GET /stream", func(rw http.ResponseWriter, r *http.Request) {
		view := live.View{Key: "page", Render: func(context.Context, *sql.Tx) (any, error) {
			return sse.PatchElements([]byte(`<p id="page">page</p>`)), nil
		}}
		hub.Serve(rw, r, live.Spec{ID: "t1", Views: []live.View{view}})
	})
	srv := httptest.NewServer(edge.Sessions{}.Handler(mux))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(t.Context(), "GET", srv.URL+"/stream", nil)
	req.Header.Set("Accept-Encoding", "identity")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	lines := bufio.NewScanner(res.Body)
	next := func(want string) {
		t.Helper()
		for lines.Scan() {
			if strings.Contains(lines.Text(), want) {
				return
			}
		}
		t.Fatalf("no line with %s", want)
	}
	next(`<p id="page">page</p>`)

	jar, _ := cookiejar.New(nil)
	session := &http.Client{Jar: jar} // one session, so that its limit holds
	post := func(body string) (int, string) {
		res, err := session.Post(srv.URL+"/cmd/cursor-party/go", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var b strings.Builder
		bufio.NewReader(res.Body).WriteTo(&b)
		return res.StatusCode, b.String()
	}
	if code, body := post(`{"tab":"t1","op":"op1"}`); code != http.StatusInternalServerError || body != "" {
		t.Fatalf("a failed write: %d %q", code, body)
	}
	next(`"_cursorParty":{"ack":"op1","problem":"That could not be saved. Try again in a moment."}`)
	if code, body := post(`{"tab":"t1","op":"op2"}`); code != http.StatusTooManyRequests || body != "" {
		t.Fatalf("over the limit: %d %q", code, body)
	}
	next(`"_cursorParty":{"ack":"op2","problem":"Too many requests at once. Wait a moment, then try again."}`)
	if code, _ := post(`{"tab":"t2","op":"op3"}`); code != http.StatusBadRequest {
		t.Fatalf("a tab the session does not hold: %d", code)
	}
}

// The expressions of a demo's pending op.
func TestPending(t *testing.T) {
	p := demo.NewPending("_guestbook")
	if f := p.LostFilter(); f != `{include: /^_guestbook\.synced$/}` {
		t.Errorf("LostFilter %s", f)
	}
	for _, want := range []string{"patch._guestbook?.synced", "$_guestbook.inflight > 0 ? ($_guestbook.recheck = true)", "'That did not go through. Try again.'"} {
		if !strings.Contains(p.LostExpr(), want) {
			t.Errorf("LostExpr lacks %s: %s", want, p.LostExpr())
		}
	}
	for _, want := range []string{"'started'", "'finished'", "'retries-failed'", "status !== 409 && +evt.detail.argsRaw.status !== 429 && +evt.detail.argsRaw.status < 500", "'That did not work. Try again.'", demo.Reopen} {
		if !strings.Contains(p.Fetched(), want) {
			t.Errorf("Fetched lacks %s: %s", want, p.Fetched())
		}
	}
}
