// Package web serves the site: the landing page, the docs, the demos and
// their streams, the static files. The edge goes around Routes in
// internal/app.
package web

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/edge"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/content"
	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/docs"
	"github.com/zweiundeins/lith/site/internal/markdown"
	"github.com/zweiundeins/lith/site/internal/views"
)

// Config is what the site needs to know about where it runs.
type Config struct {
	// Base is the site's public URL, without a trailing /: canonical
	// links, the Open Graph image and the sitemap start with it.
	Base string
	// Repo is the repository's URL on GitHub.
	Repo    string
	Version string
	// Dev shows draft docs pages, and reads the docs from ./content/docs
	// on every request when that directory exists.
	Dev bool
}

// Server holds what the handlers share.
type Server struct {
	cfg    Config
	db     *sqlite.DB
	w      *writer.Writer
	hub    *live.Hub
	log    *slog.Logger
	static *edge.Static
	assets views.Assets
	docs   *docs.Docs
	demos  []site
	// commands limits every action of the site per client address.
	commands *edge.Limiter
}

// site is a demo as the site serves it, its notes rendered.
type site struct {
	demo.Demo
	card        views.Card
	server, how templ.Component
}

// New returns the server of the database that w writes and hub renders,
// with the demos given.
func New(cfg Config, db *sqlite.DB, w *writer.Writer, hub *live.Hub, log *slog.Logger, list []demo.Demo) (*Server, error) {
	s := &Server{cfg: cfg, db: db, w: w, hub: hub, log: log, commands: edge.NewLimiter(20, 60)}
	var err error
	if s.docs, err = docs.Load(content.Docs, cfg.Dev); err != nil {
		return nil, fmt.Errorf("docs: %w", err)
	}
	for _, d := range list {
		art := d.Art
		if art == "" {
			art = views.LogoSVG()
		}
		server, err := markdown.Render([]byte(d.Server))
		if err != nil {
			return nil, err
		}
		how, err := markdown.Render([]byte(d.How))
		if err != nil {
			return nil, err
		}
		s.demos = append(s.demos, site{
			Demo:   d,
			card:   views.Card{Slug: d.Slug, Title: d.Title, Summary: d.Summary, Art: art},
			server: templ.Raw(server.HTML),
			how:    templ.Raw(how.HTML),
		})
	}
	if err := s.loadStatic(list); err != nil {
		return nil, err
	}
	return s, nil
}

// Routes returns the handlers, without the edge.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /stream", s.homeStream)
	mux.HandleFunc("GET /docs", s.doc)
	mux.HandleFunc("GET /docs/{path...}", s.doc)
	mux.HandleFunc("GET /demos", s.demoIndex)
	mux.HandleFunc("GET /demos/{slug}", s.demoPage)
	mux.HandleFunc("GET /demos/{slug}/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/demos/"+r.PathValue("slug"), http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /demos/{slug}/stream", s.demoStream)
	mux.Handle("GET /static/", s.static)
	mux.HandleFunc("GET /favicon.ico", s.file("image/png", iconPNG32))
	mux.HandleFunc("GET /apple-touch-icon.png", s.file("image/png", iconPNG180))
	mux.HandleFunc("GET /og.png", s.file("image/png", socialPNG))
	mux.HandleFunc("GET /robots.txt", s.robots)
	mux.HandleFunc("GET /sitemap.xml", s.sitemap)
	deps := demo.Deps{DB: s.db, W: s.w, Hub: s.hub, Log: s.log, Commands: s.commands}
	for _, d := range s.demos {
		if d.Mount != nil {
			d.Mount(demo.NewKit(d.Slug, mux, deps))
		}
	}
	mux.HandleFunc("/", s.notFound)
	return mux
}

// shell is the frame of a page for this request.
func (s *Server) shell(r *http.Request, title, description, path, nav string) views.Shell {
	return views.Shell{
		Title: title, Description: description, Path: path, Base: s.cfg.Base, Nav: nav,
		Theme:  pref(r, "lith-theme", "dark", "light", "system"),
		Style:  pref(r, "lith-style", "pixel", "smooth"),
		Assets: s.assets, Nonce: edge.Nonce(r.Context()), Version: s.cfg.Version, Repo: s.cfg.Repo,
	}
}

// pref reads a display preference from its cookie: one of the values, the
// first by default.
func pref(r *http.Request, cookie string, values ...string) string {
	if c, err := r.Cookie(cookie); err == nil && slices.Contains(values, c.Value) {
		return c.Value
	}
	return values[0]
}

const description = "lith is the runtime of the Go, SQLite and Datastar apps of zwei und eins: one writer, views rendered once per tick for everyone, and HTML streamed over SSE."

// home is the landing page. Its slots show the demos' embeds, drawn from
// one read transaction and kept current by the page's stream.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sh := s.shell(r, "", description, "/", "")
	viewer := demo.Viewer{Session: edge.SessionID(ctx), Tab: demo.NewID()}
	if s.embeds(viewer) != nil {
		sh.Stream, sh.Tab = "/stream", viewer.Tab
	}
	cards := s.cards()
	s.page(w, r, sh, func(ctx context.Context, tx *sql.Tx) (templ.Component, error) {
		slots := map[string]templ.Component{}
		for _, d := range s.demos {
			if d.Embed == nil {
				continue
			}
			c, err := d.Embed.Render(ctx, tx, viewer)
			if err != nil {
				return nil, fmt.Errorf("%s embed: %w", d.Slug, err)
			}
			slots[string(d.Embed.Slot)] = island(d.Slug, c)
		}
		return views.LandingPage(views.Landing{Slots: slots, Cards: cards, Repo: s.cfg.Repo}), nil
	})
}

// embeds are the views of the landing page's slots, nil if none streams.
func (s *Server) embeds(v demo.Viewer) []live.View {
	var vs []live.View
	for _, d := range s.demos {
		if d.Embed != nil {
			vs = append(vs, d.Embed.Views(v)...)
		}
	}
	return vs
}

func (s *Server) homeStream(w http.ResponseWriter, r *http.Request) {
	viewer, ok := demo.Who(r, r.URL.Query().Get("tab"))
	if !ok {
		http.Error(w, "bad tab id", http.StatusBadRequest)
		return
	}
	vs := s.embeds(viewer)
	if vs == nil {
		w.WriteHeader(http.StatusNoContent) // nothing to stream: retry 'error' stops at a 204
		return
	}
	s.stream(w, r, viewer, vs)
}

func (s *Server) demoIndex(w http.ResponseWriter, r *http.Request) {
	sh := s.shell(r, "Demos", "Live demos of lith: each runs on this server, with the code beside it.", "/demos", "demos")
	s.page(w, r, sh, func(context.Context, *sql.Tx) (templ.Component, error) {
		return views.DemoIndex(s.cards()), nil
	})
}

func (s *Server) cards() []views.Card {
	cards := make([]views.Card, len(s.demos))
	for i, d := range s.demos {
		cards[i] = d.card
	}
	return cards
}

func (s *Server) demo(slug string) *site {
	for i := range s.demos {
		if s.demos[i].Slug == slug {
			return &s.demos[i]
		}
	}
	return nil
}

// demoPage is a demo's page: the demo drawn from one read transaction, and
// the stream that keeps it current.
func (s *Server) demoPage(w http.ResponseWriter, r *http.Request) {
	d := s.demo(r.PathValue("slug"))
	if d == nil {
		s.notFound(w, r)
		return
	}
	viewer := demo.Viewer{Session: edge.SessionID(r.Context()), Tab: demo.NewID()}
	sh := s.shell(r, d.Title, d.Summary, "/demos/"+d.Slug, "demos")
	sh.Stream, sh.Tab = "/demos/"+d.Slug+"/stream", viewer.Tab
	s.page(w, r, sh, func(ctx context.Context, tx *sql.Tx) (templ.Component, error) {
		stage, err := d.Page.Render(ctx, tx, viewer)
		if err != nil {
			return nil, err
		}
		return views.Demo(views.DemoPage{Card: d.card, Server: d.server, How: d.how, Stage: island(d.Slug, stage)}), nil
	})
}

func (s *Server) demoStream(w http.ResponseWriter, r *http.Request) {
	d := s.demo(r.PathValue("slug"))
	if d == nil {
		http.Error(w, "no such demo", http.StatusNotFound)
		return
	}
	viewer, ok := demo.Who(r, r.URL.Query().Get("tab"))
	if !ok {
		http.Error(w, "bad tab id", http.StatusBadRequest)
		return
	}
	s.stream(w, r, viewer, d.Page.Views(viewer))
}

// stream serves the tab's stream of views until the tab leaves or the
// server stops; then its last event re-creates the page's stream element,
// which opens it again on the next process.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, v demo.Viewer, vs []live.View) {
	err := s.hub.Serve(w, r, live.Spec{ID: v.Tab, Views: vs, Reconnect: views.StreamID, WriteTick: frameWriter()})
	if err != nil && !errors.Is(err, live.ErrFull) && !errors.Is(err, live.ErrStopped) && !errors.Is(err, sse.ErrNoRoom) {
		s.log.Debug("stream ended", "tab", v.Tab, "err", err)
	}
}

// frameWriter writes a tick's frames to a stream in one write: a []byte
// as it is, and a demo.Frame's First the first time the stream sends its
// view, its Next after that.
func frameWriter() func(*sse.Stream, []live.Frame) error {
	seen := map[string]bool{}
	var events [][]byte
	return func(st *sse.Stream, frames []live.Frame) error {
		events = events[:0]
		for _, f := range frames {
			switch v := f.Value.(type) {
			case []byte:
				events = append(events, v)
			case demo.Frame:
				if seen[f.Key] {
					events = append(events, v.Next)
				} else {
					seen[f.Key] = true
					events = append(events, v.First)
				}
			}
		}
		if len(events) == 0 {
			return nil
		}
		return st.Write(events...)
	}
}

// island puts a demo's part in its element, the root of its @scope.
func island(slug string, c templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if _, err := io.WriteString(w, `<div class="island" data-demo="`+slug+`">`); err != nil {
			return err
		}
		if err := c.Render(ctx, w); err != nil {
			return err
		}
		_, err := io.WriteString(w, `</div>`)
		return err
	})
}

// doc is a page of the docs.
func (s *Server) doc(w http.ResponseWriter, r *http.Request) {
	if p := r.URL.Path; len(p) > len("/docs") && strings.HasSuffix(p, "/") {
		http.Redirect(w, r, strings.TrimRight(p, "/"), http.StatusMovedPermanently)
		return
	}
	d := s.docsFor()
	p := d.Page(r.URL.Path)
	if p == nil {
		s.notFound(w, r)
		return
	}
	title := p.Title
	if p == d.Home {
		title = "Docs"
	}
	sh := s.shell(r, title, p.Summary, p.Path, "docs")
	s.page(w, r, sh, func(context.Context, *sql.Tx) (templ.Component, error) {
		return views.Docs(views.DocsPage{Docs: d, Page: p, Edit: s.cfg.Repo + "/blob/main/site/content/docs/" + p.File}), nil
	})
}

// docsFor returns the docs: in development read again from the working
// directory when it has them, so an edit shows on the next reload.
func (s *Server) docsFor() *docs.Docs {
	if !s.cfg.Dev {
		return s.docs
	}
	if fi, err := os.Stat("content/docs"); err != nil || !fi.IsDir() {
		return s.docs
	}
	d, err := docs.Load(os.DirFS("content/docs"), true)
	if err != nil {
		s.log.Warn("docs", "err", err)
		return s.docs
	}
	return d
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	sh := s.shell(r, "Not found", "This page does not exist.", r.URL.Path, "")
	s.render(w, r, http.StatusNotFound, sh, views.NotFound())
}

// page renders a document in one read transaction, and writes nothing.
func (s *Server) page(w http.ResponseWriter, r *http.Request, sh views.Shell, body func(context.Context, *sql.Tx) (templ.Component, error)) {
	ctx := r.Context()
	var buf bytes.Buffer
	err := s.db.View(ctx, func(tx *sql.Tx) error {
		c, err := body(ctx, tx)
		if err != nil {
			return err
		}
		return views.Document(sh, c).Render(ctx, &buf)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.send(w, http.StatusOK, sh, buf.Bytes())
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, code int, sh views.Shell, c templ.Component) {
	var buf bytes.Buffer
	if err := views.Document(sh, c).Render(r.Context(), &buf); err != nil {
		s.fail(w, r, err)
		return
	}
	s.send(w, code, sh, buf.Bytes())
}

func (s *Server) send(w http.ResponseWriter, code int, sh views.Shell, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Add("Vary", "Cookie")
	if sh.Tab != "" {
		h.Set("Cache-Control", "no-store") // the tab id is this document's alone
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	w.WriteHeader(code)
	w.Write(body)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, "Something went wrong.", http.StatusInternalServerError)
}

// file serves bytes made once, precompressed where that helps.
func (s *Server) file(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		edge.ServePrecompressed(w, r, body)
	}
}
