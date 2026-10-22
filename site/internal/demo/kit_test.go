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
