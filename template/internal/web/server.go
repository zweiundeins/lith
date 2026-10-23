// Package web serves the guestbook: its page, the tab's stream, the action,
// and the routes that pace a stream whose page falls behind, and its static
// files. cmd/app puts the edge around Routes, and serves Static outside
// the sessions.
package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/zweiundeins/lith/edge"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/minify"
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
	assets string // the version of the page's scripts and stylesheet
	limit  *edge.Limiter
	// /pace and /ack are limited per client address first, then per
	// session or stream: a client mints a session with every request
	// that sends no cookie, so a session id alone is no limiter key.
	paceAddrs, paces *edge.Limiter
	ackAddrs, acks   *edge.Limiter
}

// New returns the server of the database that w writes and hub renders.
// It serves its scripts and stylesheets minified, or as written with dev,
// for the browser's devtools.
func New(db *sqlite.DB, w *writer.Writer, hub *live.Hub, log *slog.Logger, dev bool) (*Server, error) {
	files, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	var opts []edge.StaticOption
	if !dev {
		opts = append(opts, edge.Minify(minify.Bytes))
	}
	static, err := edge.NewStatic(files, "/static/", opts...)
	if err != nil {
		return nil, err
	}
	// lith's newest-frame.js, which the page loads after Datastar.
	js, err := fs.ReadFile(live.Scripts, live.NewestFrame)
	if err != nil {
		return nil, err
	}
	static.Add("lith/"+live.NewestFrame, js)
	assets := sha256.Sum256([]byte(static.URL("style.css") + " " + static.URL("vendor/datastar-rocket.js") + " " + static.URL("lith/"+live.NewestFrame)))
	return &Server{
		db: db, w: w, hub: hub, log: log, static: static,
		assets:    hex.EncodeToString(assets[:6]),
		limit:     edge.NewLimiter(0.5, 5),  // per client address: five signatures at once, then one every two seconds
		paceAddrs: edge.NewLimiter(4, 20),   // per client address: a few people's tabs
		paces:     edge.NewLimiter(1, 5),    // per session: a page reports every 3 s at most
		ackAddrs:  edge.NewLimiter(80, 160), // per client address: four paced streams
		acks:      edge.NewLimiter(20, 40),  // per stream: a paced page acknowledges ten writes a second at most
	}, nil
}

// Routes returns the handlers, without the edge and without the static
// files.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /stream", s.stream)
	mux.HandleFunc("POST /cmd/sign", s.sign)
	mux.HandleFunc("POST /pace", s.pace)
	mux.HandleFunc("POST /ack", s.ack)
	return mux
}

// Static serves the static files at /static/. Mount it outside the
// sessions: its answers are public and immutable, and a session cookie on
// one would go to everyone a shared cache gives it to.
func (s *Server) Static() *edge.Static { return s.static }

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
			Tab:         newID(),
			Build:       s.hub.Build(),
			Assets:      s.assets,
			Nonce:       edge.Nonce(ctx),
			CSS:         s.static.URL("style.css"),
			JS:          s.static.URL("vendor/datastar-rocket.js"),
			NewestFrame: s.static.URL("lith/" + live.NewestFrame),
			Entries:     list,
			Total:       total,
		}).Render(ctx, &buf)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")    // the tab id is this document's alone
	h.Set("Accept-CH", edge.NetworkHints) // Chromium's hints for edge.SlowLink
	w.Write(buf.Bytes())
}

// pageState is what the stream's request reports in its signals, for a
// refresh after a deploy (views.openStream): the build and the assets
// version that rendered the page, and whether the person typed.
type pageState struct {
	Build  string `json:"_build"`
	Assets string `json:"_assets"`
	Dirty  bool   `json:"_dirty"`
}

// stream sends the tab its views: the shared entries, then its own state.
// A tab on a slow link gets each view's frames about four times a second.
// The stream sends every frame until /pace paces it. It counts under the
// client's address, so one address cannot hold every stream
// (live.Options.MaxStreamsPerClient). A page that a former build rendered
// is refreshed first (refresh).
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	var page pageState
	if !validID(tab) || sse.ReadSignals(w, r, &page) != nil {
		w.WriteHeader(http.StatusBadRequest) // no page of ours sends it
		return
	}
	sid := edge.SessionID(r.Context())
	spec := live.Spec{
		ID:        streamID(sid, tab),
		Client:    edge.ClientKey(r),
		Views:     []live.View{entriesView(), tabView(sid, tab)},
		Reconnect: "stream",
		Slow:      edge.SlowLink(r),
		Ack:       ackEvent(tab),
		Build:     page.Build,
		Page:      r.Header.Get("Page-Path"),
		Dirty:     page.Dirty,
		Refresh: func(ctx context.Context, rq live.RefreshRequest) ([][]byte, error) {
			return s.refresh(ctx, rq, page.Assets)
		},
	}
	if err := s.hub.Serve(w, r, spec); err != nil {
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
// or a refusal, reaches the page on the tab's stream. A status code
// carries nothing a person reads: what no command decided, a rate limit or
// a failed write, reaches the tab as a notice with the op id.
func (s *Server) sign(w http.ResponseWriter, r *http.Request) {
	var req signRequest
	if err := sse.ReadSignalsLimit(w, r, &req, 8<<10); err != nil || !validID(req.Tab) || !validID(req.Op) {
		w.WriteHeader(http.StatusBadRequest) // no page of ours sends it
		return
	}
	sid := edge.SessionID(r.Context())
	id := streamID(sid, req.Tab)
	// A tab whose stream is not open would never see the outcome, and its
	// row would come from a tab id the client made up. The page waits on a
	// 409 as on a 5xx: its stream connects again, the tab's row lacks the
	// op, and the page tells the person in its own words.
	if !s.hub.HasStream(id) {
		w.WriteHeader(http.StatusConflict)
		return
	}
	if !s.limit.Allow(edge.ClientKey(r), 1, time.Now()) {
		s.notify(r.Context(), id, req.Op, "Too many signatures at once. Wait a moment, then try again.")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	cmd := guestbook.Sign{Session: sid, Tab: req.Tab, Op: req.Op, Name: req.Name, Message: req.Message, At: time.Now()}
	switch err := s.w.Do(r.Context(), writer.Once(sid+":"+req.Op, cmd)); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, writer.ErrInvalid):
		w.WriteHeader(http.StatusBadRequest) // Sign refuses what a person can type: this is a bug
	case errors.Is(err, writer.ErrClosed):
		w.WriteHeader(http.StatusServiceUnavailable) // the hub stopped first: Stopping told the tab
	case r.Context().Err() != nil:
		// The client went away: nobody waits for an answer or a notice.
	default:
		s.log.Error("sign failed", "err", err)
		s.notify(r.Context(), id, req.Op, "Your signature could not be saved. Try again in a moment.")
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// notify sends the tab's stream a notice in place of the outcome of its
// action op. The tab's view puts the outcome back with its next frame,
// and a stream that reconnects never gets it.
func (s *Server) notify(ctx context.Context, streamID, op, msg string) {
	ev, err := notice(ctx, op, msg)
	if err != nil {
		s.log.Error("notice", "err", err)
		return
	}
	s.hub.Notify(streamID, "notice", ev)
}

// fail logs err and answers the document with 500, which the person reads.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, "Something went wrong. Reload the page in a moment.", http.StatusInternalServerError)
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
