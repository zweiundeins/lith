// Package web serves the guestbook: its page, the tab's stream and the
// action. cmd/app puts the edge around Routes.
package web

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/base64"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/zweiundeins/lith/edge"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/template/internal/guestbook"
	"github.com/zweiundeins/lith/template/internal/views"
)

//go:embed static
var staticFiles embed.FS

// Server holds what the handlers share.
type Server struct {
	db     *sqlite.DB
	w      *writer.Writer
	hub    *live.Hub
	log    *slog.Logger
	static *edge.Static
	limit  *edge.Limiter
}

// New returns the server of the database that w writes and hub renders.
func New(db *sqlite.DB, w *writer.Writer, hub *live.Hub, log *slog.Logger) (*Server, error) {
	files, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	static, err := edge.NewStatic(files, "/static/")
	if err != nil {
		return nil, err
	}
	return &Server{
		db: db, w: w, hub: hub, log: log, static: static,
		limit: edge.NewLimiter(0.5, 5), // per client address: five signatures at once, then one every two seconds
	}, nil
}

// Routes returns the handlers, without the edge.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /stream", s.stream)
	mux.HandleFunc("POST /cmd/sign", s.sign)
	mux.Handle("GET /static/", s.static)
	return mux
}

// page renders the document in one read transaction, and writes nothing:
// the tab is new, and its row appears with its first signature.
func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var buf bytes.Buffer
	err := s.db.View(ctx, func(tx *sql.Tx) error {
		list, total, err := guestbook.Latest(ctx, tx, guestbook.Shown)
		if err != nil {
			return err
		}
		return views.Guestbook(views.Page{
			Tab:     newID(),
			CSS:     s.static.URL("style.css"),
			JS:      s.static.URL("vendor/datastar-rocket.js"),
			Entries: list,
			Total:   total,
		}).Render(ctx, &buf)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store") // the tab id is this document's alone
	w.Write(buf.Bytes())
}

// stream sends the tab its views: the shared entries, then its own state.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	if !validID(tab) {
		http.Error(w, "bad tab id", http.StatusBadRequest)
		return
	}
	sid := edge.SessionID(r.Context())
	if err := s.hub.Serve(w, r, live.Spec{ID: tab, Views: []live.View{entriesView(), tabView(sid, tab)}, Reconnect: "stream"}); err != nil {
		s.log.Debug("stream ended", "tab", tab, "err", err)
	}
}

type signRequest struct {
	Tab     string `json:"tab"`
	Op      string `json:"op"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

// sign answers 204 once the signature is committed. Its outcome, an entry
// or a refusal, reaches the page on the tab's stream.
func (s *Server) sign(w http.ResponseWriter, r *http.Request) {
	if !s.limit.Allow(edge.ClientKey(r), 1, time.Now()) {
		http.Error(w, "too many signatures, wait a moment", http.StatusTooManyRequests)
		return
	}
	var req signRequest
	if err := sse.ReadSignalsLimit(w, r, &req, 8<<10); err != nil || !validID(req.Tab) || !validID(req.Op) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	sid := edge.SessionID(r.Context())
	cmd := guestbook.Sign{Session: sid, Tab: req.Tab, Op: req.Op, Name: req.Name, Message: req.Message, At: time.Now()}
	err := s.w.Do(r.Context(), writer.Once(sid+":"+req.Op, cmd))
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, writer.ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, writer.ErrClosed):
		http.Error(w, "the server is restarting", http.StatusServiceUnavailable)
	default:
		s.fail(w, r, err)
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, "something went wrong", http.StatusInternalServerError)
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
