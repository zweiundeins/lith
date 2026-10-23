package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"slices"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"

	"github.com/zweiundeins/lith/template/internal/guestbook"
	"github.com/zweiundeins/lith/template/internal/views"
)

// entriesView is the shared region: rendered once per change, and the same
// frame goes to every tab.
func entriesView() live.View {
	return live.View{
		Key:    "entries",
		Topics: []string{guestbook.Topic},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			list, total, err := guestbook.Latest(ctx, tx, guestbook.Shown)
			if err != nil {
				return nil, err
			}
			return patch(ctx, views.Entries(list, total))
		},
	}
}

// ack tells the page which op of the form's key (sign) the tab's outcome
// belongs to, and whether it was refused. The tab view sets Synced: the
// stream the page opened has brought the tab's row.
type ack struct {
	Ops    map[string]opAck `json:"_ops"`
	Synced bool             `json:"_synced,omitempty"`
}

type opAck struct {
	Op      string `json:"ack"`
	Refused bool   `json:"refused"`
}

// tabView is the tab's own region, rendered from its row in tab_state, with
// the op id that ends the page's pending state.
func tabView(sid, tab string) live.View {
	key := guestbook.TabTopic(tab)
	return live.View{
		Key:    key,
		Topics: []string{key},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			state, err := guestbook.Tab(ctx, tx, sid, tab)
			if err != nil {
				return nil, err
			}
			html, err := patch(ctx, views.Outcome(state))
			if err != nil {
				return nil, err
			}
			signals, err := json.Marshal(ack{Ops: map[string]opAck{"sign": {Op: state.Op, Refused: state.Refusal != ""}}, Synced: true})
			if err != nil {
				return nil, err
			}
			return slices.Concat(html, sse.PatchSignals(signals, false)), nil
		},
	}
}

// notice is a notice for the tab's action op (live.Hub.Notify): the
// message in the outcome's place, and the op id, refused, which ends
// the page's pending state and keeps what the person typed. Without an op
// it leaves the signals alone.
func notice(ctx context.Context, op, msg string) ([]byte, error) {
	html, err := patch(ctx, views.Notice(msg))
	if err != nil || op == "" {
		return html, err
	}
	signals, err := json.Marshal(ack{Ops: map[string]opAck{"sign": {Op: op, Refused: true}}})
	if err != nil {
		return nil, err
	}
	return slices.Concat(html, sse.PatchSignals(signals, false)), nil
}

// Stopping is the hub's live.Options.Stopping: a tab's stream ends with a
// notice in the outcome's place, which the next process's first frame of
// the tab's view replaces.
func Stopping(live.Spec) [][]byte {
	ev, err := notice(context.Background(), "", "The server is updating. Back in a moment.")
	if err != nil {
		return nil
	}
	return [][]byte{ev}
}

// refresh is the stream's live.Spec.Refresh, for a page that a former
// build rendered: it renders #content afresh, while the stream's element,
// #ops and the signals stay outside it. When the person has typed into the
// form, or the page's scripts and stylesheet changed, it asks for a reload
// instead: a morph would take what was typed, or leave new markup with the
// old stylesheet. assets is the assets version the page reported.
func (s *Server) refresh(ctx context.Context, rq live.RefreshRequest, assets string) ([][]byte, error) {
	var msg string
	switch {
	case rq.Dirty:
		msg = views.UpdateTyped
	case assets != s.assets:
		msg = views.UpdateAssets
	}
	if msg != "" {
		ev, err := patch(ctx, views.Update(msg))
		return [][]byte{ev}, err
	}
	if u, err := url.Parse(rq.Page); err != nil || u.Path != "/" {
		return nil, nil // not a page of ours
	}
	var buf bytes.Buffer
	err := s.db.View(ctx, func(tx *sql.Tx) error {
		list, total, err := guestbook.Latest(ctx, tx, guestbook.Shown)
		if err != nil {
			return err
		}
		return views.Content(list, total).Render(ctx, &buf)
	})
	if err != nil {
		return nil, err
	}
	return [][]byte{sse.PatchElements(buf.Bytes())}, nil
}

// patch renders c as an event that morphs it into the page by its id.
func patch(ctx context.Context, c templ.Component) ([]byte, error) {
	var buf bytes.Buffer
	if err := c.Render(ctx, &buf); err != nil {
		return nil, err
	}
	return sse.PatchElements(buf.Bytes()), nil
}
