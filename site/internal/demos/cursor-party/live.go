package cursorparty

import (
	"context"
	"database/sql"
	"time"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// views are what a tab at the party watches, on the demo's page and on
// the landing page alike: the layer and the counter. Both are shared:
// nothing in them depends on the viewer, so each renders once a tick for
// every tab.
func views(demo.Viewer) []live.View {
	return []live.View{cursorsView(), countView()}
}

// cursorsView is the layer of cursors. It renders in every tick in which
// someone moved, and once a second without moves, so that quiet cursors
// fade and leave. Its frames come up to 20 times a second: they morph
// without a view transition, and CSS eases each cursor to its new place.
func cursorsView() live.View {
	return live.View{
		Key:    cursorsKey,
		Topics: []string{Topic},
		Every:  time.Second,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			now := time.Now()
			list, err := cursors(ctx, demo.Querier(ctx, tx), now)
			if err != nil {
				return nil, err
			}
			return sse.PatchElements(layer(list, now)), nil
		},
	}
}

// countView is the counter: the tabs at the party, which the hub counts
// (Presence renders it again when one comes or goes), and the moves of
// the last second. It renders with the layer, but its frame changes once
// a second at most, and a frame equal to the last one is not sent.
func countView() live.View {
	return live.View{
		Key:      countKey,
		Topics:   []string{Topic},
		Every:    time.Second,
		Presence: true,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			rate, err := movesPerSecond(ctx, demo.Querier(ctx, tx), time.Now())
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, Count(here(), rate))
		},
	}
}

// here is how many tabs watch the party, on both pages.
func here() int {
	if h := hub.Load(); h != nil {
		return h.Count(countKey)
	}
	return 0
}

// renderPage draws the party on the demo's page.
func renderPage(ctx context.Context, tx *sql.Tx, v demo.Viewer) (templ.Component, error) {
	html, rate, err := party(ctx, tx)
	if err != nil {
		return nil, err
	}
	return Stage(selfStyle(v.Tab), html, arriving(), rate), nil
}

// renderEmbed draws the party behind the landing page's title.
func renderEmbed(ctx context.Context, tx *sql.Tx, v demo.Viewer) (templ.Component, error) {
	html, rate, err := party(ctx, tx)
	if err != nil {
		return nil, err
	}
	return Hero(selfStyle(v.Tab), html, arriving(), rate), nil
}

// party reads what a page starts with, in the document's transaction:
// the layer as the views render it, and the moves of the last second.
func party(ctx context.Context, tx *sql.Tx) (html []byte, rate int, err error) {
	now := time.Now()
	q := demo.Querier(ctx, tx)
	list, err := cursors(ctx, q, now)
	if err != nil {
		return nil, 0, err
	}
	rate, err = movesPerSecond(ctx, q, now)
	return layer(list, now), rate, err
}

// arriving is the count a new page starts with: the tabs there already,
// and at least this one. Its stream's frames then count it, so the number
// rises as the tab arrives instead of falling back first.
func arriving() int { return max(here(), 1) }
