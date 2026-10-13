package live

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"
)

// Watchers lists the IDs of the streams on a view in the order they
// opened, each once and without the streams that have none, and a
// Presence view that reads it renders again as they come and go.
func TestWatchers(t *testing.T) {
	var h *Hub
	h = start(t, Options{Tick: 5 * time.Millisecond})
	v := text("here", nil, func(context.Context, *sql.Tx) (string, error) {
		return strings.Join(h.Watchers("here"), " "), nil
	})
	v.Presence = true
	if ids := h.Watchers("here"); ids != nil {
		t.Fatalf("an unwatched view has watchers %q", ids)
	}
	a := serve(h, Spec{ID: "a", Views: []View{v}}, "")
	waitFor(t, "a", func() bool { return a.has(`<p id="here">a</p>`) })
	anon := serve(h, Spec{Views: []View{v}}, "")
	b := serve(h, Spec{ID: "b", Views: []View{v}}, "")
	waitFor(t, "a and b", func() bool { return a.has(`<p id="here">a b</p>`) && b.has(`<p id="here">a b</p>`) })
	if n := h.Count("here"); n != 3 {
		t.Fatalf("Count %d, want 3 with the stream without an ID", n)
	}
	anon.leave(t)
	a.leave(t)
	waitFor(t, "b alone", func() bool { return b.has(`<p id="here">b</p>`) })
	if ids := h.Watchers("here"); !slices.Equal(ids, []string{"b"}) {
		t.Fatalf("watchers %q", ids)
	}
	b.leave(t)
	if ids := h.Watchers("here"); ids != nil {
		t.Fatalf("watchers %q after everyone left", ids)
	}
}
