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

// patch renders c as an event that morphs it into the page by its id.
func patch(ctx context.Context, c templ.Component) ([]byte, error) {
	var buf bytes.Buffer
	if err := c.Render(ctx, &buf); err != nil {
		return nil, err
	}
	return sse.PatchElements(buf.Bytes()), nil
}
