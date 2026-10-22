package town

import (
	"context"
	"database/sql"
	"slices"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// renderPage draws the demo into the GET document: a new tab is at now.
func renderPage(ctx context.Context, tx *sql.Tx, v demo.Viewer) (templ.Component, error) {
	q := demo.Querier(ctx, tx)
	b, err := bounds(ctx, q)
	if err != nil {
		return nil, err
	}
	m, err := momentWithin(ctx, q, b, 0)
	if err != nil {
		return nil, err
	}
	return Stage(b, m, whoOf(v.Session)), nil
}

// pageViews are what a tab's stream watches from the start: its own view
// and the slider's end. The town comes when the page answers the own
// view's first frame (Follow): the town now, or as of the tab's position.
func pageViews(v demo.Viewer) []live.View {
	return []live.View{tabView(v), headView()}
}

// viewsAt are the views of the town at a position: the town now (0), or
// the town as of p and the events after p. final says that those events
// are all there, so that no new event changes them.
func viewsAt(p int64, final bool) []live.View {
	if p == 0 {
		return []live.View{nowView()}
	}
	return []live.View{atView(p), laterView(p, final)}
}

// keysAt are the keys of viewsAt(p).
func keysAt(p int64) []string {
	if p == 0 {
		return []string{nowKey}
	}
	return []string{atKey(p), laterKey(p)}
}

// tabView is the tab's own view: the outcome of its last change, and the
// position the server has it at, with the number of the seek that put it
// there, which ends the page's pending seek. Its first frame on a stream
// also asks the page to say hello (Follow).
func tabView(v demo.Viewer) live.View {
	key := demo.TabTopic(Slug, v.Tab)
	return live.View{
		Key:    key,
		Topics: []string{key},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			var s TabState
			if err := demo.LoadTab(ctx, demo.Querier(ctx, tx), Slug, v, &s); err != nil {
				return nil, err
			}
			out, err := demo.Patch(ctx, Outcome(s))
			if err != nil {
				return nil, err
			}
			var a ack
			a.Town.At, a.Town.Seq, a.Town.Ack, a.Town.OK, a.Town.Synced = s.At, s.Seq, s.Op, s.Op != "" && s.Refusal == "", true
			signals, err := demo.Signals(a)
			if err != nil {
				return nil, err
			}
			next := append(out, signals...)
			return demo.Frame{First: append(slices.Clip(next), hello...), Next: next}, nil
		},
	}
}

// ack is the tab's state as the page's signals take it.
type ack struct {
	Town struct {
		At     int64  `json:"at"`
		Seq    int64  `json:"seq"`
		Ack    string `json:"ack"`
		OK     bool   `json:"ok"`
		Synced bool   `json:"synced"` // the stream the page opened brought the tab's row (demo.Pending)
	} `json:"_town"`
}

var hello = sse.PatchSignals([]byte(`{"_town":{"hello":true}}`), false)

// headView is the slider's ends, the same for every tab. For a tab in the
// past it is what new events render again, with the events after a
// position near the end (laterView).
func headView() live.View {
	return live.View{
		Key:    headKey,
		Topics: []string{Topic, TopicPruned},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			b, err := bounds(ctx, demo.Querier(ctx, tx))
			if err != nil {
				return nil, err
			}
			return demo.Signals(map[string]any{"_town": map[string]int64{"first": b.First, "head": b.Head}})
		},
	}
}

// nowView is the town now, for every tab at the slider's end: the tiles,
// the newest events, and no events after them.
func nowView() live.View {
	return live.View{
		Key:    nowKey,
		Topics: []string{Topic, TopicPruned},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			m, err := momentAt(ctx, demo.Querier(ctx, tx), 0)
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, Moment(m))
		},
	}
}

// atView is the town as of position p, for every tab at p: the tiles
// from the history table, and the events up to p. Nothing after p changes
// it, so it listens to no new event: only to the sweep, which can drop
// the events of p.
func atView(p int64) live.View {
	return live.View{
		Key:    atKey(p),
		Topics: []string{TopicPruned},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			m, err := momentAt(ctx, demo.Querier(ctx, tx), p)
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, Moment(m))
		},
	}
}

// laterView is the events after position p. Once there are Later of them
// they are final; until then new events add to them, so the view listens
// to the town while the tab that first watched it found it unfinished.
func laterView(p int64, final bool) live.View {
	topics := []string{TopicPruned}
	if !final {
		topics = append(topics, Topic)
	}
	return live.View{
		Key:    laterKey(p),
		Topics: topics,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			evs, err := eventsAfter(ctx, demo.Querier(ctx, tx), p, Later)
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, LaterList(evs, people{}))
		},
	}
}

// moment is the town at a position as the page shows it.
type moment struct {
	At      int64 // 0: now
	Town    Town
	Earlier []Event // the newest first, the one at the position first of all
	// Gone is a position before the slider's start, which the sweep
	// compacted: the page shows the start instead.
	Gone   int64
	people people
}

// momentAt reads the town at position p, or now (0).
func momentAt(ctx context.Context, q sqlite.Querier, p int64) (moment, error) {
	b, err := bounds(ctx, q)
	if err != nil {
		return moment{}, err
	}
	return momentWithin(ctx, q, b, p)
}

// momentWithin reads the town at position p, or now (0), within the
// slider's bounds b: now from town_tiles, a position from the history
// table.
func momentWithin(ctx context.Context, q sqlite.Querier, b Bounds, p int64) (moment, error) {
	m := moment{At: p, people: people{}}
	var err error
	if p == 0 {
		m.Town, err = townNow(ctx, q)
		if err != nil || b.Head == 0 {
			return m, err
		}
		m.Earlier, err = eventsUpTo(ctx, q, b.Head, Earlier)
		return m, err
	}
	if b.Head == 0 {
		return m, nil
	}
	if p < b.First {
		m.Gone, p = p, b.First
	}
	if m.Town, err = townAt(ctx, q, p); err != nil {
		return m, err
	}
	m.Earlier, err = eventsUpTo(ctx, q, p, Earlier)
	return m, err
}
