package board

import (
	"context"
	"database/sql"
	"sync/atomic"
	"time"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// hub is the site's live hub, which Mount hands over. The presence view
// and the sweep ask it whose stream is open, and the board's view how
// many streams watch it. Nil until Mount (in a test of the views alone).
var hub atomic.Pointer[live.Hub]

// watching is how many streams watch the view with the key.
func watching(key string) int {
	if h := hub.Load(); h != nil {
		return h.Count(key)
	}
	return 0
}

// streamOpen reports whether the tab's stream is open: every tab of the
// page watches its own view, keyed by its tab.
func streamOpen(tab string) bool { return watching(demo.TabTopic(Slug, tab)) > 0 }

// renderPage draws the demo into the GET document. The stream's first
// frames bring the same.
func renderPage(ctx context.Context, tx *sql.Tx, v demo.Viewer) (templ.Component, error) {
	q := demo.Querier(ctx, tx)
	b, err := readBoard(ctx, q)
	if err != nil {
		return nil, err
	}
	people, err := present(ctx, q, streamOpen)
	if err != nil {
		return nil, err
	}
	n := track.numbers(time.Now(), watching(boardKey))
	return Stage(b, n, people, WhoOf(v.Session), time.Now()), nil
}

// pageViews are what a tab of the demo's page watches: the board, its
// numbers and who is here, the same for everyone, and the tab's own
// outcome. The board comes first, so that a move's frame and its outcome
// reach the tab in this order, in one write.
func pageViews(v demo.Viewer) []live.View {
	return []live.View{boardView(), statsView(), hereView(), tabView(v)}
}

// boardView renders the board once per tick that changed it, for every
// tab. Its later frames morph inside a view transition, so the cards
// glide to their new places; each also tells the page the board's
// version, which ends a pending move once it shows the move.
func boardView() live.View {
	return live.View{
		Key:    boardKey,
		Topics: []string{Topic},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			b, err := readBoard(ctx, demo.Querier(ctx, tx))
			if err != nil {
				return nil, err
			}
			f, err := demo.Animated(ctx, LanesView(b, time.Now()), sse.Patch{})
			if err != nil {
				return nil, err
			}
			v, err := demo.Signals(versionSignal(b.Version))
			if err != nil {
				return nil, err
			}
			f.First, f.Next = append(f.First, v...), append(f.Next, v...)
			track.rendered(time.Now(), b.Version, watching(boardKey))
			return f, nil
		},
	}
}

func versionSignal(v int64) any {
	type board struct {
		V int64 `json:"v"`
	}
	return struct {
		Board board `json:"_board"`
	}{board{v}}
}

// statsView renders the board's numbers every second, and when a tab
// comes or goes. A second whose numbers did not change sends nothing:
// the hub drops a frame equal to the last.
func statsView() live.View {
	return live.View{
		Key:      statsKey,
		Every:    time.Second,
		Presence: true,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			return demo.Patch(ctx, Stats(track.numbers(time.Now(), watching(boardKey))))
		},
	}
}

// hereView renders who is on the page: when a tab says hello, and when a
// stream starts or stops watching it.
func hereView() live.View {
	return live.View{
		Key:      hereKey,
		Topics:   []string{HereTopic},
		Presence: true,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			people, err := present(ctx, demo.Querier(ctx, tx), streamOpen)
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, Here(people))
		},
	}
}

// ack tells the page which move the tab's outcome belongs to.
type ack struct {
	Board struct {
		Ack  string `json:"ack"`
		AckV int64  `json:"ackv"`
		OK   bool   `json:"ok"`
	} `json:"_board"`
}

// tabView is the tab's own region: what became of its last move, from its
// row in site_tabs, with the op id and the board version that end the
// page's pending state.
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
			a.Board.Ack, a.Board.AckV, a.Board.OK = state.Op, state.Version, state.Refusal == ""
			signals, err := demo.Signals(a)
			if err != nil {
				return nil, err
			}
			return append(html, signals...), nil
		},
	}
}

// renderEmbed draws the landing page's slot: the board in miniature, its
// numbers and who is on it.
func renderEmbed(ctx context.Context, tx *sql.Tx, v demo.Viewer) (templ.Component, error) {
	q := demo.Querier(ctx, tx)
	b, err := readBoard(ctx, q)
	if err != nil {
		return nil, err
	}
	people, err := present(ctx, q, streamOpen)
	if err != nil {
		return nil, err
	}
	return Embed(b, track.numbers(time.Now(), watching(boardKey)), people, time.Now()), nil
}

// embedViews are what the landing page's stream watches for the slot. The
// miniature morphs without a view transition: the landing page has other
// live parts, which a transition would hold still.
func embedViews(v demo.Viewer) []live.View {
	return []live.View{miniView(), statsView(), hereView()}
}

func miniView() live.View {
	return live.View{
		Key:    miniKey,
		Topics: []string{Topic},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			b, err := readBoard(ctx, demo.Querier(ctx, tx))
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, Mini(b, time.Now()))
		},
	}
}
