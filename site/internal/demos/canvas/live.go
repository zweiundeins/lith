package canvas

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
	"time"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// The views' keys. None carries anything of the viewer: every painter's
// stream shares all three.
const (
	boardKey = Slug + ":board"
	hereKey  = Slug + ":here"
	pulseKey = Slug + ":pulse"
)

// renderPage draws the canvas into the GET document from its read
// transaction: the views' parts as they are now, the tab among the
// painters (its stream opens after the document), and the brush its
// session used last.
func renderPage(ctx context.Context, tx *sql.Tx, v demo.Viewer) (templ.Component, error) {
	q := demo.Querier(ctx, tx)
	cells, in, err := cellsOf(ctx, q)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tabs := in.watchers(hereKey)
	if !slices.Contains(tabs, v.Tab) {
		tabs = append(tabs, v.Tab)
	}
	list, err := painters(ctx, q, tabs, now)
	if err != nil {
		return nil, err
	}
	p, err := pulse(ctx, q, now)
	if err != nil {
		return nil, err
	}
	color, err := lastColor(ctx, q, v.Session)
	if err != nil {
		return nil, err
	}
	// The tab's first pick, which the page sends as it starts, brings its
	// brush to the painters; the document draws it so already.
	me := Tag(v.Tab)
	for i := range list {
		if list[i].Tag == me && list[i].Color < 0 {
			list[i].Color = color
		}
	}
	return Stage(Page{Cells: cells, Painters: list, Pulse: p, Color: color, Me: me}), nil
}

// pageViews are what a painter's stream watches: the board, the painters
// here and the pulse, the same for every painter.
func pageViews(demo.Viewer) []live.View {
	return []live.View{boardView(), hereView(), pulseView()}
}

// boardView is the canvas: rendered once per tick in which a cell changed,
// for every painter, from the board encoded once per version. A paint that
// changes no colour does not render it.
func boardView() live.View {
	return live.View{
		Key:    boardKey,
		Topics: []string{TopicCells},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			cells, _, err := cellsOf(ctx, demo.Querier(ctx, tx))
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, Board(cells))
		},
	}
}

// hereView lists the painters whose streams watch it: Presence renders it
// again when one comes or goes, the painters' topic when one picks a
// colour, and every two seconds the counts of the last minute move on. A
// render whose frame equals the last sends nothing.
func hereView() live.View {
	return live.View{
		Key:      hereKey,
		Topics:   []string{TopicPainters},
		Presence: true,
		Every:    2 * time.Second,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			q := demo.Querier(ctx, tx)
			in, err := instanceIn(ctx, q)
			if err != nil {
				return nil, err
			}
			list, err := painters(ctx, q, in.watchers(hereKey), time.Now())
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, Here(list))
		},
	}
}

// pulseView is the last minute: rendered in the ticks with paints, and
// every second while the minute moves on.
func pulseView() live.View {
	return live.View{
		Key:    pulseKey,
		Topics: []string{TopicPaints},
		Every:  time.Second,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			p, err := pulse(ctx, demo.Querier(ctx, tx), time.Now())
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, Activity(p))
		},
	}
}

// instanceIn finds the process's state of the canvas the transaction
// reads.
func instanceIn(ctx context.Context, q sqlite.Querier) (*instance, error) {
	var epoch int64
	if err := q.QueryRowContext(ctx, `SELECT epoch FROM canvas_board WHERE id = 1`).Scan(&epoch); err != nil {
		return nil, err
	}
	return instanceOf(epoch), nil
}

// you marks the tab's own face in the list of painters. It is the tab's,
// so the document renders it once; the shared view only names each face
// by its tag.
func you(tag string) templ.Component {
	return templ.Raw(`<style>@scope ([data-demo="canvas"]) { .cv__painter[data-tag="` + tag + `"] { --_ring: var(--lith-accent); --_you: "you" } }</style>`)
}

func hereTitle(n int) string {
	if n == 1 {
		return "1 painter here"
	}
	return strconv.Itoa(n) + " painters here"
}

func shown(list []Painter) []Painter {
	if len(list) > Shown {
		return list[:Shown]
	}
	return list
}

func describe(p Painter) string {
	s := "Painter " + p.Tag[:4]
	if p.Color >= 0 {
		s += ", " + colorNames[p.Color]
	}
	if p.Pixels > 0 {
		s += ", " + plural(p.Pixels, "pixel", "pixels") + " in the last minute"
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// bar is a second's bar in the pulse, its height a share of the busiest
// second's, with 20 pixels as the least full height.
func bar(n, peak int) string {
	if n == 0 {
		return "--h: 0"
	}
	return "--h: " + strconv.FormatFloat(float64(n)/float64(max(peak, 20)), 'f', 2, 64)
}

// thousands writes n with a comma every three digits: 12,345.
func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
