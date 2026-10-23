package demo

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/edge"
	"github.com/zweiundeins/lith/sse"
)

// An animated frame's transition is scoped to an element: one of the whole
// document would take every click on the page while it runs.
func TestAnimatedNeedsAScope(t *testing.T) {
	c := templ.Raw(`<p id="x">x</p>`)
	if _, err := Animated(context.Background(), c, sse.Patch{}); err == nil {
		t.Fatal("Animated took a patch without ViewTransitionSelector")
	}
	f, err := Animated(context.Background(), c, sse.Patch{ViewTransitionSelector: "#x"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(f.First, []byte("ViewTransition")) {
		t.Errorf("the first frame has a transition: %q", f.First)
	}
	if !bytes.Contains(f.Next, []byte("data: useViewTransition true\ndata: viewTransitionSelector #x\n")) {
		t.Errorf("a later frame has no scoped transition: %q", f.Next)
	}
}

// A Separate command leaves the site's limit for actions to the others.
func TestSeparateCommand(t *testing.T) {
	mux := http.NewServeMux()
	k := NewKit("party", mux, Deps{Commands: edge.NewLimiter(1, 1)})
	ok := func(w http.ResponseWriter, r *http.Request) {
		if Limited(r) {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
	k.Action("move", Rate{Every: time.Millisecond, Burst: 100, Separate: true}, ok)
	k.Action("sign", Rate{Every: time.Second, Burst: 5}, ok)
	post := func(name string) int {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/cmd/party/"+name, nil))
		return w.Code
	}
	for range 10 {
		if c := post("move"); c != http.StatusNoContent {
			t.Fatalf("a move got %d", c)
		}
	}
	if c := post("sign"); c != http.StatusNoContent {
		t.Fatalf("after the moves, an action got %d", c)
	}
	if c := post("sign"); c != http.StatusTooManyRequests {
		t.Fatalf("the site's limit let a second action through: %d", c)
	}
}

// An address gets Rate.Sessions sessions' worth of a command, four by
// default, however many sessions it makes up: each request without a
// cookie gets a new one.
func TestRateSessions(t *testing.T) {
	mux := http.NewServeMux()
	k := NewKit("town", mux, Deps{Commands: edge.NewLimiter(1000, 1000)})
	ok := func(w http.ResponseWriter, r *http.Request) {
		if Limited(r) {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
	k.Action("seek", Rate{Every: time.Hour, Burst: 2, Separate: true, Sessions: 1}, ok)
	k.Action("place", Rate{Every: time.Hour, Burst: 2}, ok)
	h := edge.Sessions{}.Handler(mux)
	allowed := func(name string) (n int) {
		for range 20 {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", "/cmd/town/"+name, nil))
			if w.Code == http.StatusNoContent {
				n++
			}
		}
		return n
	}
	if n := allowed("seek"); n != 2 {
		t.Errorf("an address made up sessions and got %d seeks, want 2", n)
	}
	if n := allowed("place"); n != 8 {
		t.Errorf("an address got %d places, want 8", n)
	}
}
