package demo

import (
	"context"

	"github.com/zweiundeins/lith/live"
)

type hubKey struct{}

// WithHub returns ctx carrying the hub that serves the site's streams. The
// site hands it to the hub's Run, so that every view's Render has it, and
// to the GET documents' renders and the sweeps.
func WithHub(ctx context.Context, h *live.Hub) context.Context {
	return context.WithValue(ctx, hubKey{}, h)
}

func hubFrom(ctx context.Context) *live.Hub {
	h, _ := ctx.Value(hubKey{}).(*live.Hub)
	return h
}

// Watching is how many streams watch the view with the key (live.Hub.Count).
// It works with the ctx of a Part's Render, of a view's Render, and of a
// Sweep (tx.Context()); without a hub, as in a test of the views alone, it
// is 0.
func Watching(ctx context.Context, key string) int {
	if h := hubFrom(ctx); h != nil {
		return h.Count(key)
	}
	return 0
}

// Watchers are the tab ids of the streams on the view, in the order they
// opened (live.Hub.Watchers); nil without a hub, as Watching.
func Watchers(ctx context.Context, key string) []string {
	if h := hubFrom(ctx); h != nil {
		return h.Watchers(key)
	}
	return nil
}
