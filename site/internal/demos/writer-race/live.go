package writerrace

import (
	"context"
	"database/sql"
	"time"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// renderPage draws the demo into the GET document: the controls, the
// track, the line and the results. The tab is new, so its outcome is
// empty.
func renderPage(ctx context.Context, tx *sql.Tx, v demo.Viewer) (templ.Component, error) {
	q := demo.Querier(ctx, tx)
	t, err := loadTrack(ctx, q)
	if err != nil {
		return nil, err
	}
	waiting, err := inLine(ctx, q)
	if err != nil {
		return nil, err
	}
	list, err := recent(ctx, q, Shown)
	if err != nil {
		return nil, err
	}
	return Stage(t, waiting, list, time.Now()), nil
}

// pageViews are what a tab of the demo's page watches: the track, the
// line and the results, the same for everyone, and the tab's own outcome.
func pageViews(v demo.Viewer) []live.View {
	return []live.View{trackView(), lineView(), resultsView(), tabView(v)}
}

// trackView is the race on the track. While a race runs, every sample
// touches LiveTopic, so the view renders once a tick: its frames morph at
// once, without a view transition, and CSS eases what moves.
func trackView() live.View {
	return live.View{
		Key:    Slug + ":track",
		Topics: []string{Topic, LiveTopic},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			t, err := loadTrack(ctx, demo.Querier(ctx, tx))
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, Track(t))
		},
	}
}

// lineView is the race on the track and the races in line. Its frames
// morph at once: a race that joins slides in, and the first in line turns
// into the one on the track. The table of results animates in a view
// transition, and two of them at once would cut the first one short.
func lineView() live.View {
	return live.View{
		Key:    Slug + ":line",
		Topics: []string{LineTopic, Topic},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			list, err := inLine(ctx, demo.Querier(ctx, tx))
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, Line(list))
		},
	}
}

// resultsView is the table of the last races: a race that ends arrives on
// top, and the others glide down.
func resultsView() live.View {
	return live.View{
		Key:    Slug + ":results",
		Topics: []string{ResultsTopic},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			list, err := recent(ctx, demo.Querier(ctx, tx), Shown)
			if err != nil {
				return nil, err
			}
			return demo.Animated(ctx, Results(list, time.Now()), sse.Patch{ViewTransitionSelector: "#writer-race-results"})
		},
	}
}

// ack tells the page which op the tab's outcome belongs to, which race is
// the tab's, and whether it still waits or runs.
type ack struct {
	WriterRace struct {
		Ack    string `json:"ack"`
		Mine   int64  `json:"mine"`
		Active bool   `json:"active"`
		Synced bool   `json:"synced"` // the stream the page opened brought the tab's row (demo.Pending)
	} `json:"_writerRace"`
}

// tabView is the tab's own region, from its row in site_tabs and its
// race's row: it listens to the line and the track as well, since its
// race moves up the line and onto the track with the others.
func tabView(v demo.Viewer) live.View { return ownView(v, Outcome) }

// ownView renders a tab's outcome with the component given, and the
// signals that end its pending state and mark its race.
func ownView(v demo.Viewer, c func(outcome) templ.Component) live.View {
	key := demo.TabTopic(Slug, v.Tab)
	return live.View{
		Key:    key,
		Topics: []string{key, LineTopic, Topic},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			o, err := loadOutcome(ctx, demo.Querier(ctx, tx), v)
			if err != nil {
				return nil, err
			}
			html, err := demo.Patch(ctx, c(o))
			if err != nil {
				return nil, err
			}
			var a ack
			a.WriterRace.Ack, a.WriterRace.Mine = o.State.Op, o.Race.ID
			a.WriterRace.Active, a.WriterRace.Synced = o.Race.ID != 0 && !o.Race.Over(), true
			signals, err := demo.Signals(a)
			if err != nil {
				return nil, err
			}
			return append(html, signals...), nil
		},
	}
}

// outcome is what the tab hears about its last start: the tab's state,
// and the race it is about with its place in line.
type outcome struct {
	State TabState
	Race  Race
	Place int
}

func loadOutcome(ctx context.Context, q sqlite.Querier, v demo.Viewer) (outcome, error) {
	var o outcome
	if err := demo.LoadTab(ctx, q, Slug, v, &o.State); err != nil || o.State.Race == 0 {
		return o, err
	}
	r, place, ok, err := raceOf(ctx, q, o.State.Race)
	if ok {
		o.Race, o.Place = r, place
	}
	return o, err
}

// renderEmbed draws the landing page's slot: the race on the track, or
// the last one.
func renderEmbed(ctx context.Context, tx *sql.Tx, v demo.Viewer) (templ.Component, error) {
	t, err := loadTrack(ctx, demo.Querier(ctx, tx))
	if err != nil {
		return nil, err
	}
	return Embed(t), nil
}

// embedViews are the slot's: the race, the same for every landing page,
// and the tab's own outcome of the button.
func embedViews(v demo.Viewer) []live.View {
	return []live.View{{
		Key:    Slug + ":embed",
		Topics: []string{Topic, LiveTopic},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			t, err := loadTrack(ctx, demo.Querier(ctx, tx))
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, embedLive(t))
		},
	}, ownView(v, EmbedOutcome)}
}
