package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"hash/maphash"
	"slices"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"

	"github.com/zweiundeins/lith/site/internal/docs/example/board"
)

// boardView is the board, one frame for everyone who watches it: its key
// and its topic are the board's, and nothing in it depends on the viewer.
func boardView(id int64) live.View {
	key := board.Topic(id)
	return live.View{
		Key:    key,
		Topics: []string{key},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			q, _ := sqlite.QuerierFrom(ctx) // the worker's prepared statements, in this render's transaction
			b, err := board.Load(ctx, q, id)
			if err != nil {
				return nil, err
			}
			return renderBoard(ctx, b)
		},
	}
}

// boardFrame is the board view's frame: each lane rendered on its own, and
// the whole board. A stream sends only the lanes that changed since its
// last frame, and the whole board when lanes came or went (tailor).
type boardFrame struct {
	Whole []byte // the board, as one event
	Shape string // the lanes' ids, in order
	Lanes []laneFrame
}

type laneFrame struct {
	ID    int64
	Hash  uint64 // of the lane's HTML
	Event []byte // the lane, as an event that morphs it in by its id
}

var hashSeed = maphash.MakeSeed()

// renderBoard renders a board's frame, once for every stream.
func renderBoard(ctx context.Context, b board.Board) (boardFrame, error) {
	var f boardFrame
	lanes, err := renderLanes(ctx, b)
	if err != nil {
		return f, err
	}
	var shape strings.Builder
	for i, html := range lanes {
		f.Lanes = append(f.Lanes, laneFrame{ID: b.Lanes[i].ID, Hash: maphash.Bytes(hashSeed, html), Event: sse.PatchElements(html)})
		shape.WriteString(strconv.FormatInt(b.Lanes[i].ID, 10) + ",")
	}
	whole, err := render(ctx, Board(lanes))
	if err != nil {
		return f, err
	}
	f.Whole, f.Shape = sse.PatchElements(whole), shape.String()
	return f, nil
}

// renderLanes renders each lane, for the board's frame and its document.
func renderLanes(ctx context.Context, b board.Board) ([][]byte, error) {
	lanes := make([][]byte, len(b.Lanes))
	for i, l := range b.Lanes {
		var next int64
		if i+1 < len(b.Lanes) {
			next = b.Lanes[i+1].ID
		}
		html, err := render(ctx, Lane(l, next))
		if err != nil {
			return nil, err
		}
		lanes[i] = html
	}
	return lanes, nil
}

// tailor is a stream's WriteTick: it writes all of a tick's frames in one
// write, one compression and one flush, and of the board only the lanes
// this stream has not seen. It keeps what it sent in its closure: one
// tailor per stream.
func tailor() func(*sse.Stream, []live.Frame) error {
	var shape string
	sent := map[int64]uint64{}
	var events [][]byte
	return func(s *sse.Stream, frames []live.Frame) error {
		events = events[:0]
		for _, f := range frames {
			switch v := f.Value.(type) {
			case []byte:
				events = append(events, v)
			case boardFrame:
				if v.Shape != shape {
					shape = v.Shape
					clear(sent)
					events = append(events, v.Whole)
					for _, l := range v.Lanes {
						sent[l.ID] = l.Hash
					}
					continue
				}
				for _, l := range v.Lanes {
					if sent[l.ID] != l.Hash {
						sent[l.ID] = l.Hash
						events = append(events, l.Event)
					}
				}
			}
		}
		if len(events) == 0 {
			return nil
		}
		return s.Write(events...)
	}
}

// presenceView says how many tabs show a board. Presence marks it dirty
// whenever a stream starts or stops watching it, and hub.Count says how
// many do: it listens to no topic.
func presenceView(hub *live.Hub, id int64) live.View {
	key := "presence:" + board.Topic(id)
	return live.View{
		Key:      key,
		Presence: true,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			return patch(ctx, Presence(hub.Count(key)))
		},
	}
}

// tabView is the tab's own region: why its last action changed nothing,
// the panel's place while none is open, and as signals the newest op of
// each op key, which ends its pending state ($_ops.<key>.ack), with
// $_synced, which tells the page that the stream it opened has brought
// the tab's row.
func tabView(sid, tab string) live.View {
	key := board.TabTopic(tab)
	return live.View{
		Key:    key,
		Topics: []string{key},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			q, _ := sqlite.QuerierFrom(ctx)
			s, err := board.Tab(ctx, q, sid, tab)
			if err != nil {
				return nil, err
			}
			events := [][]byte{}
			parts := []templ.Component{Outcome(s)}
			if s.Panel == 0 {
				parts = append(parts, NoPanel())
			}
			for _, c := range parts {
				html, err := patch(ctx, c)
				if err != nil {
					return nil, err
				}
				events = append(events, html)
			}
			ops := s.Ops
			if ops == nil {
				ops = map[string]board.Ack{} // null would delete the page's $_ops
			}
			ack, err := json.Marshal(map[string]any{"_ops": ops, "_synced": true})
			if err != nil {
				return nil, err
			}
			return slices.Concat(append(events, sse.PatchSignals(ack, false))...), nil
		},
	}
}

// notice is a notice for the tab's action op under its op key
// (live.Hub.Notify): the message in the outcome's place, and the op id,
// refused, which ends the page's pending state and keeps what the person
// typed (settled). An action without an op leaves the signals alone.
func notice(ctx context.Context, key, op, msg string) ([]byte, error) {
	html, err := patch(ctx, Notice(msg))
	if err != nil || op == "" {
		return html, err
	}
	ack, err := json.Marshal(map[string]any{"_ops": map[string]board.Ack{key: {Op: op, Refused: true}}})
	if err != nil {
		return nil, err
	}
	return slices.Concat(html, sse.PatchSignals(ack, false)), nil
}

// stopping is the hub's Options.Stopping: a tab's stream ends with a
// notice in the outcome's place, which the next process's first frame of
// the tab's view replaces. The status page's stream has no ID and no
// outcome to put it in.
func stopping(spec live.Spec) [][]byte {
	if spec.ID == "" {
		return nil
	}
	ev, err := notice(context.Background(), "", "", "The server is updating. Back in a moment.")
	if err != nil {
		return nil
	}
	return [][]byte{ev}
}

// refresh is the stream's live.Spec.Refresh, for a page that a former
// build rendered: #content rendered afresh, as the GET renders it, and the
// views' first frames after it. A page with a title the person typed gets
// a notice that asks for a reload instead.
func (s *Server) refresh(ctx context.Context, rq live.RefreshRequest) ([][]byte, error) {
	if rq.Dirty {
		ev, err := patch(ctx, Update())
		return [][]byte{ev}, err
	}
	path, _, _ := strings.Cut(rq.Page, "?")
	rest, ok := strings.CutPrefix(path, "/boards/")
	id, err := strconv.ParseInt(rest, 10, 64)
	if !ok || err != nil {
		return nil, nil // not a board's page
	}
	return s.db.ViewValue(ctx, func(tx *sql.Tx) ([][]byte, error) {
		b, err := board.Load(ctx, tx, id)
		if errors.Is(err, board.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		lanes, err := renderLanes(ctx, b)
		if err != nil {
			return nil, err
		}
		ev, err := patch(ctx, Content(b, lanes))
		return [][]byte{ev}, err
	})
}

// prefsView patches the session's preferences into every tab of the
// session: one view per session, which SetCompact's topic marks dirty.
func prefsView(sid string) live.View {
	key := board.SessionTopic(sid)
	return live.View{
		Key:    key,
		Topics: []string{key},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			q, _ := sqlite.QuerierFrom(ctx)
			p, err := board.SessionPrefs(ctx, q, sid)
			if err != nil {
				return nil, err
			}
			signals, err := json.Marshal(p)
			if err != nil {
				return nil, err
			}
			return sse.PatchSignals(signals, false), nil
		},
	}
}

// cardView is a card's panel, shared by every tab that has it open.
func cardView(id int64) live.View {
	key := board.CardTopic(id)
	return live.View{
		Key:    key,
		Topics: []string{key},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			q, _ := sqlite.QuerierFrom(ctx)
			c, err := board.LoadCard(ctx, q, id)
			if err != nil {
				return nil, err
			}
			return patch(ctx, Panel(c))
		},
	}
}

// patch renders c as an event that morphs it into the page by its id.
func patch(ctx context.Context, c templ.Component) ([]byte, error) {
	html, err := render(ctx, c)
	if err != nil {
		return nil, err
	}
	return sse.PatchElements(html), nil
}

func render(ctx context.Context, c templ.Component) ([]byte, error) {
	var buf bytes.Buffer
	err := c.Render(ctx, &buf)
	return buf.Bytes(), err
}
