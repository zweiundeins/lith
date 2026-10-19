package access_test

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/docs/example/access"
)

const (
	alice, bob = 1, 2
	open       = "Holidays"
	secret     = "Salaries 2027"
)

// start opens a space with an open page and a restricted one that only
// alice may read, and serves its stream with the views that view makes.
func start(t *testing.T, view func(person, space int64) live.View) (url string, w *writer.Writer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "wiki.db"), sqlite.Options{Migrations: access.Migrations()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	hub := live.New(live.Options{Read: db.R, Tick: 5 * time.Millisecond})
	w, err = writer.New(db, writer.Options{OnCommit: hub.Changed})
	if err != nil {
		t.Fatal(err)
	}
	go w.Run(context.Background())
	t.Cleanup(func() { w.Close() })
	go hub.Run(ctx)

	err = w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
		_, err := tx.Exec(`INSERT INTO spaces (id, name) VALUES (1, 'Handbook')`)
		return err
	}))
	for _, cmd := range []writer.Command{
		access.AddPage{Space: 1, Title: open},
		access.AddPage{Space: 1, Title: secret, Restricted: true},
		access.Grant{Person: alice, Page: 2},
	} {
		if err == nil {
			err = w.Do(ctx, cmd)
		}
	}
	if err != nil {
		t.Fatal(err)
	}

	s := &access.Server{Person: func(r *http.Request) (int64, bool) {
		p, err := strconv.ParseInt(r.Header.Get("X-Person"), 10, 64) // a session, in an app
		return p, err == nil
	}}
	mux := http.NewServeMux()
	if view == nil {
		s.Hub = hub
		mux.HandleFunc("GET /spaces/{space}/stream", s.Stream)
	} else {
		mux.HandleFunc("GET /spaces/{space}/stream", func(w http.ResponseWriter, r *http.Request) {
			person, _ := s.Person(r)
			hub.Serve(w, r, live.Spec{Views: []live.View{view(person, 1)}})
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/spaces/1/stream", w
}

// watch opens person's stream and returns its frames, one string each.
func watch(t *testing.T, url string, person int64) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("X-Person", strconv.FormatInt(person, 10))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	frames := make(chan string, 16)
	go func() {
		defer res.Body.Close()
		defer close(frames)
		lines := bufio.NewScanner(res.Body)
		var event strings.Builder
		for lines.Scan() {
			if lines.Text() != "" {
				event.WriteString(lines.Text() + "\n")
				continue
			}
			if strings.Contains(event.String(), "<ul") {
				frames <- event.String()
			}
			event.Reset()
		}
	}()
	return frames
}

func next(t *testing.T, frames <-chan string) string {
	t.Helper()
	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatal("the stream ended")
		}
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("no frame for 5 s")
	}
	return ""
}

// TestPagesPerPerson watches one route as two people: each gets the list
// their own grants allow, and a grant reaches only its person's list.
func TestPagesPerPerson(t *testing.T) {
	url, w := start(t, nil)
	a, b := watch(t, url, alice), watch(t, url, bob)
	fa, fb := next(t, a), next(t, b)
	if !strings.Contains(fa, secret) || !strings.Contains(fa, open) {
		t.Errorf("alice's frame lacks a page she may read:\n%s", fa)
	}
	if strings.Contains(fb, secret) || !strings.Contains(fb, open) {
		t.Errorf("bob's frame is not the open page alone:\n%s", fb)
	}

	if err := w.Do(context.Background(), access.Grant{Person: bob, Page: 2}); err != nil {
		t.Fatal(err)
	}
	if fb := next(t, b); !strings.Contains(fb, secret) {
		t.Errorf("bob's frame after his grant:\n%s", fb)
	}
	select {
	case f := <-a:
		t.Errorf("bob's grant rendered alice's list again:\n%s", f)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestRouteKeyLeaks is the mistake the person in the key prevents: a key
// of the route alone makes the two people's views one view, rendered by
// the definition of whoever watched first, so bob sees alice's titles.
func TestRouteKeyLeaks(t *testing.T) {
	routeOnly := func(person, space int64) live.View {
		v := access.PagesView(person, space, "")
		v.Key = fmt.Sprintf("pages:%d", space) // the person is missing
		return v
	}
	url, _ := start(t, routeOnly)
	a := watch(t, url, alice)
	next(t, a)
	if fb := next(t, watch(t, url, bob)); !strings.Contains(fb, secret) {
		t.Fatalf("bob's stream did not get alice's frame; the leak this test shows is gone:\n%s", fb)
	}
}
