package stream

import (
	"context"
	"database/sql"
	"time"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/live"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// renderPage draws the demo into the GET document: the readout, which the
// stream's first report fills in, the controls and the tab's table.
func renderPage(ctx context.Context, tx *sql.Tx, v demo.Viewer) (templ.Component, error) {
	var st TabState
	if err := demo.LoadTab(ctx, demo.Querier(ctx, tx), Slug, v, &st); err != nil {
		return nil, err
	}
	return Stage(st, payloadFor(st, StepAt(time.Now()))), nil
}

// pageViews are what a tab of the demo's page watches: the report, which
// rides along with every write of the stream, the tab's choices and its
// table.
func pageViews(v demo.Viewer) []live.View {
	return []live.View{reportView(false), tabView(v), tableView(v)}
}

// reportView is the same for every tab: its frame is a probe, which each
// stream's writer keeps and asks for a report of that stream at every
// write. Rendering it again every second gives a stream that fell quiet
// its last report.
func reportView(embed bool) live.View {
	key := Slug + ":report"
	if embed {
		key += ":embed"
	}
	p := probe{embed: embed}
	return live.View{
		Key:   key,
		Every: time.Second,
		Render: func(context.Context, *sql.Tx) (any, error) {
			return p, nil
		},
	}
}

// choices are the tab's payload, and the op id of its last change.
type choices struct {
	S struct {
		Size   Size   `json:"size"`
		Churn  Churn  `json:"churn"`
		Ack    string `json:"ack"`
		Synced bool   `json:"synced"` // the stream the page opened brought the tab's row (demo.Pending)
	} `json:"_stream"`
}

// tabView patches the tab's choices into the page's signals, which press
// the controls' buttons, with the op id that ends the pending state.
func tabView(v demo.Viewer) live.View {
	key := demo.TabTopic(Slug, v.Tab)
	return live.View{
		Key:    key,
		Topics: []string{key},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			var st TabState
			if err := demo.LoadTab(ctx, demo.Querier(ctx, tx), Slug, v, &st); err != nil {
				return nil, err
			}
			var c choices
			c.S.Size, c.S.Churn, c.S.Ack, c.S.Synced = st.size(), st.churn(), st.Op, true
			return demo.Signals(c)
		},
	}
}

// tableView is the tab's table: every Step a new frame, or at once when
// the tab's choices change. A frame it has sent already, a still table's,
// renders to nil, so the stream sends nothing and can fall quiet.
func tableView(v demo.Viewer) live.View {
	type shown struct {
		size  Size
		churn Churn
		step  int64
	}
	var last shown
	return live.View{
		Key:    Slug + ":table:" + v.Tab,
		Topics: []string{demo.TabTopic(Slug, v.Tab)},
		Every:  Step,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			var st TabState
			if err := demo.LoadTab(ctx, demo.Querier(ctx, tx), Slug, v, &st); err != nil {
				return nil, err
			}
			now := shown{st.size(), st.churn(), StepAt(time.Now())}
			if now.churn == Still {
				now.step = st.Frozen
			}
			if now == last {
				return nil, nil
			}
			b, err := tableFor(ctx, st, now.step)
			if err == nil {
				last = now
			}
			return b, err
		},
	}
}

// renderEmbed draws the landing page's slot, which the stream's reports
// fill in.
func renderEmbed(ctx context.Context, tx *sql.Tx, v demo.Viewer) (templ.Component, error) {
	return Embed(), nil
}

func embedViews(v demo.Viewer) []live.View {
	return []live.View{reportView(true)}
}
