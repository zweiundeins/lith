package demo

import (
	"bytes"
	"context"
	"testing"

	"github.com/a-h/templ"
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
