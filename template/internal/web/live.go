package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"sync"
	"time"

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
// old stylesheet. assets is the assets version the page reported. After a
// deploy every open tab refreshes within a second or two: the tabs of one
// page share a render of its container (s.contents).
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
	u, err := url.Parse(rq.Page)
	if err != nil || u.Path != "/" {
		return nil, nil // not a page of ours
	}
	// The key holds what the render depends on: the page. An app with
	// languages adds the request's language. The render runs under the
	// ctx of the tab that came first, so it reads nothing from ctx that
	// the key does not hold: no nonce, a locale or a user only in the key.
	ev, err := s.contents.do(ctx, u.Path, func() (ev []byte, err error) {
		err = s.db.View(ctx, func(tx *sql.Tx) error {
			list, total, err := guestbook.Latest(ctx, tx, guestbook.Shown)
			if err != nil {
				return err
			}
			ev, err = patch(ctx, views.Content(list, total))
			return err
		})
		return ev, err
	})
	if err != nil {
		return nil, err
	}
	return [][]byte{ev}, nil
}

// sharedFor is how long a refresh's render of a container serves the tabs
// that refresh after it. The views' first frames, which follow a refresh,
// bring anything newer.
const sharedFor = time.Second

// shared lets concurrent refreshes of one key share a render: the first
// renders, the others wait for it, and those that come within sharedFor
// after it take it too. A failed render is not shared: a caller that
// waited for it renders itself. A render is dropped once sharedFor has
// passed, at the next call's sweep, so keys by session or user hold
// memory only while their tabs refresh.
type shared struct {
	mu    sync.Mutex
	calls map[string]*sharedCall
	swept time.Time
}

type sharedCall struct {
	done chan struct{} // closed once the render returned
	at   time.Time     // when it returned, under shared.mu
	ev   []byte        // read-only once done is closed
	err  error
}

func (s *shared) do(ctx context.Context, key string, render func() ([]byte, error)) ([]byte, error) {
	for {
		s.mu.Lock()
		if now := time.Now(); now.Sub(s.swept) >= sharedFor {
			s.swept = now
			for k, c := range s.calls {
				if !c.at.IsZero() && now.Sub(c.at) >= sharedFor {
					delete(s.calls, k)
				}
			}
		}
		c := s.calls[key]
		if c == nil || !c.at.IsZero() && time.Since(c.at) >= sharedFor {
			c = &sharedCall{done: make(chan struct{})}
			if s.calls == nil {
				s.calls = map[string]*sharedCall{}
			}
			s.calls[key] = c
			s.mu.Unlock()
			return s.run(key, c, render)
		}
		s.mu.Unlock()
		select {
		case <-c.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if c.err == nil {
			return c.ev, nil
		}
	}
}

// errNoRender stands for a render that panicked, until it returns.
var errNoRender = errors.New("web: the shared render panicked")

func (s *shared) run(key string, c *sharedCall, render func() ([]byte, error)) ([]byte, error) {
	c.err = errNoRender
	defer func() {
		s.mu.Lock()
		c.at = time.Now()
		if c.err != nil && s.calls[key] == c {
			delete(s.calls, key)
		}
		s.mu.Unlock()
		close(c.done)
	}()
	c.ev, c.err = render()
	return c.ev, c.err
}

// patch renders c as an event that morphs it into the page by its id. The
// HTML goes into the event as templ writes it (sse.Elements), in a buffer
// the size of the view's last frame (live.FrameBuffer), without a copy in
// between. templ buffers its writes itself, 4 KiB at a time.
func patch(ctx context.Context, c templ.Component) ([]byte, error) {
	e := sse.NewElements(live.FrameBuffer(ctx))
	if err := c.Render(ctx, e); err != nil {
		return nil, err
	}
	return e.Event(), nil
}
