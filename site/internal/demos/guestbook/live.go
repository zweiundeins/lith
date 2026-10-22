package guestbook

import (
	"context"
	"database/sql"
	"time"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// renderPage draws the demo into the GET document: the form, the tab's
// outcome (empty: the tab is new) and the entries.
func renderPage(ctx context.Context, tx *sql.Tx, v demo.Viewer) (templ.Component, error) {
	list, total, err := latest(ctx, demo.Querier(ctx, tx), Shown)
	if err != nil {
		return nil, err
	}
	return Stage(list, total, time.Now()), nil
}

// pageViews are what a tab of the demo's page watches: the entries, the
// same for everyone, and the tab's own outcome.
func pageViews(v demo.Viewer) []live.View {
	return []live.View{entriesView(), tabView(v)}
}

// entriesView is the shared region: rendered once per change for every tab,
// its morph inside a view transition, so the entries already there glide
// down while the new one arrives.
func entriesView() live.View {
	return live.View{
		Key:    Slug + ":entries",
		Topics: []string{Topic},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			list, total, err := latest(ctx, demo.Querier(ctx, tx), Shown)
			if err != nil {
				return nil, err
			}
			return demo.Animated(ctx, Entries(list, total, time.Now()), sse.Patch{ViewTransitionSelector: "#guestbook-entries"})
		},
	}
}

// ack tells the page which op the tab's outcome belongs to, and that the
// stream it opened has brought the tab's row (demo.Pending).
type ack struct {
	Guestbook struct {
		Ack      string `json:"ack"`
		Accepted bool   `json:"accepted"`
		Synced   bool   `json:"synced"`
	} `json:"_guestbook"`
}

// tabView is the tab's own region, from its row in site_tabs, with the op
// id that ends the page's pending state.
func tabView(v demo.Viewer) live.View {
	key := demo.TabTopic(Slug, v.Tab)
	return live.View{
		Key:    key,
		Topics: []string{key},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			var state TabState
			if err := demo.LoadTab(ctx, demo.Querier(ctx, tx), Slug, v, &state); err != nil {
				return nil, err
			}
			html, err := demo.Patch(ctx, Outcome(state))
			if err != nil {
				return nil, err
			}
			var a ack
			a.Guestbook.Ack, a.Guestbook.Accepted, a.Guestbook.Synced = state.Op, state.Accepted(), true
			signals, err := demo.Signals(a)
			if err != nil {
				return nil, err
			}
			return append(html, signals...), nil
		},
	}
}

// renderEmbed draws the newest entries for the landing page's slot.
func renderEmbed(ctx context.Context, tx *sql.Tx, v demo.Viewer) (templ.Component, error) {
	list, total, err := latest(ctx, demo.Querier(ctx, tx), Latest)
	if err != nil {
		return nil, err
	}
	return Embed(list, total, time.Now()), nil
}

func embedViews(v demo.Viewer) []live.View {
	return []live.View{{
		Key:    Slug + ":latest",
		Topics: []string{Topic},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			list, total, err := latest(ctx, demo.Querier(ctx, tx), Latest)
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, latestList(list, total, time.Now()))
		},
	}}
}
