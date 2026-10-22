// Package web serves the board: its page, the tab's stream and the
// actions. The wiring (package app) puts the edge around Routes.
package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/zweiundeins/lith/edge"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/docs/example/board"
)

// Server holds what the handlers share.
type Server struct {
	db       *sqlite.DB
	w        *writer.Writer
	hub      *live.Hub
	log      *slog.Logger
	limit    *edge.Limiter
	datastar string
}

// New returns the server of the database that w writes and hub renders.
// datastar is the URL of Datastar's script.
func New(db *sqlite.DB, w *writer.Writer, hub *live.Hub, log *slog.Logger, datastar string) *Server {
	return &Server{
		db: db, w: w, hub: hub, log: log, datastar: datastar,
		limit: edge.NewLimiter(5, 20), // per client address: 20 actions at once, then 5 a second
	}
}

// Routes returns the handlers, without the edge.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boards/{id}", s.page)
	mux.HandleFunc("GET /boards/{id}/stream", s.stream)
	mux.HandleFunc("POST /cmd/boards", s.createBoard)
	mux.HandleFunc("POST /cmd/add", s.add)
	mux.HandleFunc("POST /cmd/move", s.move)
	mux.HandleFunc("POST /cmd/panel", s.panel)
	mux.HandleFunc("POST /cmd/compact", s.compact)
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("GET /status/stream", s.statusStream)
	return mux
}

// page renders the document from one read transaction and writes
// nothing: the tab is new, and its row appears with its first action.
func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	var buf bytes.Buffer
	err = s.db.View(ctx, func(tx *sql.Tx) error {
		b, err := board.Load(ctx, tx, id)
		if err != nil {
			return err
		}
		prefs, err := board.SessionPrefs(ctx, tx, edge.SessionID(ctx))
		if err != nil {
			return err
		}
		lanes, err := renderLanes(ctx, b)
		if err != nil {
			return err
		}
		return Page(PageData{Tab: newID(), Datastar: s.datastar, Board: b, Lanes: lanes, Prefs: prefs}).Render(ctx, &buf)
	})
	switch {
	case errors.Is(err, board.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store") // the tab id is this document's alone
	w.Write(buf.Bytes())
}

// stream sends the tab its views: the board and who is on it, the tab's
// own region, the session's preferences, and the panel the tab's row says
// is open, so a page that reconnects after a deploy gets its panel back.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	tab := r.URL.Query().Get("tab")
	if err != nil || !validID(tab) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	sid := edge.SessionID(ctx)
	state, err := s.db.ViewValue(ctx, func(tx *sql.Tx) (board.TabState, error) { return board.Tab(ctx, tx, sid, tab) })
	if err != nil {
		s.log.Error("stream failed", "tab", tab, "err", err)
		w.WriteHeader(http.StatusInternalServerError) // the page's retry asks again
		return
	}
	views := []live.View{boardView(id), presenceView(s.hub, id), tabView(sid, tab), prefsView(sid)}
	if state.Panel != 0 {
		views = append(views, cardView(state.Panel))
	}
	err = s.hub.Serve(w, r, live.Spec{ID: streamID(sid, tab), Views: views, WriteTick: tailor(), Reconnect: StreamID})
	if err != nil {
		s.log.Debug("stream ended", "tab", tab, "err", err)
	}
}

// action is what the page's actions send.
type action struct {
	Tab     string `json:"tab"`
	Key     string `json:"key"` // the op key: the add form, a card
	Op      string `json:"op"`
	Lane    int64  `json:"lane"`
	Card    int64  `json:"card"`
	Seen    int64  `json:"seen"`
	Title   string `json:"title"`
	Name    string `json:"name"`
	Compact bool   `json:"compact"`
}

// decode reads an action and checks the address's rate limit. op says
// whether the action needs an op key and id. A body that does not parse,
// or ids that no page of ours makes, get a bare 400: that is a bug or a
// forged request, and the page ends its op in its own words. The limit is
// sized so that nobody clicking reaches it: an action over it gets 429 for
// the page's retry, and its tab a notice.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, op bool) (action, bool) {
	var a action
	err := sse.ReadSignalsLimit(w, r, &a, 8<<10)
	if err != nil || !validID(a.Tab) || op && (!validID(a.Key) || !validID(a.Op)) {
		w.WriteHeader(http.StatusBadRequest)
		return a, false
	}
	if !op {
		a.Key, a.Op = "", "" // unchecked: they name no notice
	}
	if !s.limit.Allow(edge.ClientKey(r), 1, time.Now()) {
		s.notify(r.Context(), a, "Too many actions at once. Wait a moment, then try again.")
		w.WriteHeader(http.StatusTooManyRequests)
		return a, false
	}
	return a, true
}

func (s *Server) add(w http.ResponseWriter, r *http.Request) {
	if a, ok := s.decode(w, r, true); ok {
		s.act(w, r, a, board.AddCard{Lane: a.Lane, Title: a.Title})
	}
}

func (s *Server) move(w http.ResponseWriter, r *http.Request) {
	if a, ok := s.decode(w, r, true); ok {
		s.act(w, r, a, board.MoveCard{Card: a.Card, Lane: a.Lane, Seen: a.Seen})
	}
}

// act runs cmd as the tab's action a.Op and answers 204 once it has
// committed. What became of it, a change or a refusal, reaches the page
// on the tab's stream. writer.Once records the op: a retried request (a
// flaky network, a double click) runs nothing again, and the tab's row
// still holds the first outcome.
func (s *Server) act(w http.ResponseWriter, r *http.Request, a action, cmd writer.Command) {
	sid := edge.SessionID(r.Context())
	act := board.Act{Session: sid, Tab: a.Tab, Key: a.Key, Op: a.Op, Cmd: cmd, At: time.Now()}
	if s.do(w, r, a, writer.Once(sid+":"+a.Op, act)) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// do hands cmd to the writer and reports whether it committed. When it
// did not, it answers with the status alone: the person reads why on the
// tab's stream.
func (s *Server) do(w http.ResponseWriter, r *http.Request, a action, cmd writer.Command) bool {
	switch err := s.w.Do(r.Context(), cmd); {
	case err == nil:
		return true
	case errors.Is(err, writer.ErrInvalid):
		w.WriteHeader(http.StatusBadRequest) // Act refuses what a person can get wrong: this is a bug
	case errors.Is(err, writer.ErrClosed):
		w.WriteHeader(http.StatusServiceUnavailable) // the hub stopped first: Options.Stopping told the tab
	default:
		s.failed(w, r, a, err)
	}
	return false
}

// panel opens a card's panel in the tab, or closes it with card 0. The
// tab's row remembers it, for the stream that reconnects; the stream open
// now watches the card's view at once, or from its start if the command
// arrives before the stream does.
func (s *Server) panel(w http.ResponseWriter, r *http.Request) {
	a, ok := s.decode(w, r, false)
	if !ok {
		return
	}
	ctx := r.Context()
	sid := edge.SessionID(ctx)
	old, err := s.db.ViewValue(ctx, func(tx *sql.Tx) (board.TabState, error) { return board.Tab(ctx, tx, sid, a.Tab) })
	if err != nil {
		s.failed(w, r, a, err)
		return
	}
	if !s.do(w, r, a, board.OpenPanel{Session: sid, Tab: a.Tab, Card: a.Card, At: time.Now()}) {
		return
	}
	if old.Panel != 0 && old.Panel != a.Card {
		s.hub.Unwatch(streamID(sid, a.Tab), board.CardTopic(old.Panel))
	}
	if a.Card != 0 {
		s.hub.Watch(streamID(sid, a.Tab), cardView(a.Card))
	}
	w.WriteHeader(http.StatusNoContent)
}

// compact sets the session's preference: every tab of the session gets
// it from prefsView, and the next document renders it.
func (s *Server) compact(w http.ResponseWriter, r *http.Request) {
	a, ok := s.decode(w, r, false)
	if !ok {
		return
	}
	if s.do(w, r, a, board.SetCompact{Session: edge.SessionID(r.Context()), Compact: a.Compact, At: time.Now()}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// createBoard creates a board and sends the page to it. The answer is the
// new board's address, so a retry must get the same one: OnceResult
// records the first run's result under the op id, and a retry gets that
// back instead of making a second board. Act records a refusal, a name
// that is too long say, in the tab's row, as it does for every action. A
// navigation is the page's own business, so it travels in the answer, as
// a signal.
func (s *Server) createBoard(w http.ResponseWriter, r *http.Request) {
	a, ok := s.decode(w, r, true)
	if !ok {
		return
	}
	sid := edge.SessionID(r.Context())
	op := writer.OnceResult(sid+":"+a.Op, board.CreateBoard{Name: a.Name})
	if !s.do(w, r, a, board.Act{Session: sid, Tab: a.Tab, Key: a.Key, Op: a.Op, Cmd: op, At: time.Now()}) {
		return
	}
	if op.Result() == nil {
		w.WriteHeader(http.StatusNoContent) // refused: the tab's view says why
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(sse.PatchSignals(fmt.Appendf(nil, `{"_goto":"/boards/%s"}`, op.Result()), false))
}

// status is a page of live numbers for the people who run the app. Put
// it behind a login, or on the metrics listener, in an app of your own.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	if err := StatusPage(s.datastar, s.w.Stats(), s.hub.Stats(), s.hub.Budget().Stats()).Render(r.Context(), &buf); err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

func (s *Server) statusStream(w http.ResponseWriter, r *http.Request) {
	s.hub.Serve(w, r, live.Spec{Views: []live.View{statsView(s.w, s.hub)}, Reconnect: "status-stream"})
}

// fail logs err and answers a document with 500, which the person reads.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, "Something went wrong.", http.StatusInternalServerError)
}

// failed logs err and answers the action a with 500. No command decided
// it, so no view can render it: the tab hears of it from a notice.
func (s *Server) failed(w http.ResponseWriter, r *http.Request, a action, err error) {
	s.log.Error("action failed", "path", r.URL.Path, "err", err)
	s.notify(r.Context(), a, "That could not be saved. Try again in a moment.")
	w.WriteHeader(http.StatusInternalServerError)
}

// notify sends the tab of action a a notice in place of its outcome, with
// the op id that ends its pending state. The tab's view puts the outcome
// back with its next frame, and a stream that reconnects never gets it.
// Each op key has a notice key of its own, so a notice for one key never
// replaces another's that has not gone out; an action without an op has
// one key for all.
func (s *Server) notify(ctx context.Context, a action, msg string) {
	ev, err := notice(ctx, a.Key, a.Op, msg)
	if err != nil {
		s.log.Error("notice", "err", err)
		return
	}
	key := "notice"
	if a.Op != "" {
		key += ":" + a.Key
	}
	s.hub.Notify(streamID(edge.SessionID(ctx), a.Tab), key, ev)
}

// streamID names a tab's stream after its session too, so that an action
// reaches only the streams of its own session: whoever learns a tab id
// can neither watch views for it nor send it a notice.
func streamID(sid, tab string) string {
	return sid + "." + tab
}

// newID returns a tab id: 128 random bits in base64url.
func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// validID accepts tab ids and the op ids pages make.
func validID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range []byte(s) {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
