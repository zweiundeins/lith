package life

import (
	"context"
	"database/sql"
	"time"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/live"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// renderPage draws the demo into the GET document: the board as it is, who
// watches it, and the colour the session draws in.
func renderPage(ctx context.Context, tx *sql.Tx, v demo.Viewer) (templ.Component, error) {
	q := demo.Querier(ctx, tx)
	b, err := load(ctx, q)
	if err != nil {
		return nil, err
	}
	colour, err := colourOf(ctx, q, v.Session)
	if err != nil {
		return nil, err
	}
	return Stage(b, demo.Watching(ctx, BoardKey), colour, time.Now()), nil
}

// pageViews are what a tab of the demo watches: the board, the same for
// everyone, and the tab's own region.
func pageViews(v demo.Viewer) []live.View {
	return []live.View{boardView(), tabView(v)}
}

// boardView is the shared view: rendered once per generation for every
// tab, without a view transition (at ten frames a second one would cut the
// last one short), when someone comes or goes (Presence), and every second
// while the board is quiet, for its countdown. Its watchers are the ones
// the clock counts.
func boardView() live.View {
	return live.View{
		Key:      BoardKey,
		Topics:   []string{Topic},
		Every:    time.Second,
		Presence: true,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			b, err := load(ctx, demo.Querier(ctx, tx))
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, Frame(b, demo.Watching(ctx, BoardKey), time.Now()))
		},
	}
}

// tab is what the tab's view patches into the page's signals: the op of
// its newest action, and the colour its session draws in.
type tab struct {
	Life struct {
		Ack    string `json:"ack"`
		Colour int    `json:"colour"`
		Synced bool   `json:"synced"` // the stream the page opened brought the tab's row (demo.Pending)
	} `json:"_life"`
}

// tabView is the tab's own region, from its row in site_tabs and its
// session's colour.
func tabView(v demo.Viewer) live.View {
	key := demo.TabTopic(Slug, v.Tab)
	return live.View{
		Key:    key,
		Topics: []string{key, sessionTopic(v.Session)},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			q := demo.Querier(ctx, tx)
			var state TabState
			if err := demo.LoadTab(ctx, q, Slug, v, &state); err != nil {
				return nil, err
			}
			var t tab
			var err error
			if t.Life.Colour, err = colourOf(ctx, q, v.Session); err != nil {
				return nil, err
			}
			t.Life.Ack, t.Life.Synced = state.Op, true
			html, err := demo.Patch(ctx, Outcome(state))
			if err != nil {
				return nil, err
			}
			signals, err := demo.Signals(t)
			if err != nil {
				return nil, err
			}
			return append(html, signals...), nil
		},
	}
}
