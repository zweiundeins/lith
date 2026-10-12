package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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

// ack tells the page which op the tab's outcome belongs to.
type ack struct {
	Op       string `json:"_ack"`
	Accepted bool   `json:"_accepted"`
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
			signals, err := json.Marshal(ack{Op: state.Op, Accepted: state.Accepted()})
			if err != nil {
				return nil, err
			}
			return slices.Concat(html, sse.PatchSignals(signals, false)), nil
		},
	}
}

// patch renders c as an event that morphs it into the page by its id.
func patch(ctx context.Context, c templ.Component) ([]byte, error) {
	var buf bytes.Buffer
	if err := c.Render(ctx, &buf); err != nil {
		return nil, err
	}
	return sse.PatchElements(buf.Bytes()), nil
}
