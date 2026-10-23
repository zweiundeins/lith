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
	"sync"
	"time"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/edge"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/content"
	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/docs"
	"github.com/zweiundeins/lith/site/internal/githistory"
	"github.com/zweiundeins/lith/site/internal/markdown"
	"github.com/zweiundeins/lith/site/internal/starbase"
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
	// Dev shows draft docs pages, reads the docs from ./content/docs on
	// every request when that directory exists, and serves scripts and
	// stylesheets as written instead of minified.
	Dev bool
	// Pacing is how the streams are paced by their pages. "" is the hybrid
	// in place, as the site runs: a stream starts unpaced and is paced once
	// its page posts /pace, which newest-frame.js has it do when the page
	// falls behind, and released once the page keeps up again. The other
	// values are for measurements (TestLifeDrift): "off" leaves
	// newest-frame.js alone, and /pace only counts the report; "every"
	// paces every stream from the start; "reopen" paces a stream that the
	// page opened again with Save-Data: on, which newest-frame.js's
	// __reopen sends, in place of the slow stream it would get.
	Pacing string
	// AckWindow is the writes a paced stream sends unacknowledged at most
	// (live.Spec.Window): 0 for 1. AckOnApply has a page acknowledge a
	// write once it applied it, instead of after the paint that follows.
	// AckRelease and AckReleaseSlack are live.Spec.Release and
	// live.Spec.ReleaseSlack, 0 for their defaults; with a negative
	// AckReleaseSlack the page does not report its round trip. For
	// measurements.
	AckWindow       int
	AckOnApply      bool
	AckRelease      time.Duration
	AckReleaseSlack time.Duration
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
	// starbase are the vendored Starbase components.
	starbase *starbase.Set
	docs     *docs.Docs
	demos    []site
	// robotsTxt and sitemapXML are made once, and served precompressed.
	robotsTxt, sitemapXML []byte
	// history is each page's last commit, by its path; empty when the
	// build had no git history.
	history map[string]githistory.Commit
	// commands limits every action of the site per client address.
	commands *edge.Limiter
	// paces limits /pace per session, acks limits /ack per stream, each
	// after paceAddrs and ackAddrs per client address: a client mints a
	// session with every request that sends no cookie, so a session id
	// alone is no limiter key.
	paces, acks         *edge.Limiter
	paceAddrs, ackAddrs *edge.Limiter
	// owners are the sessions of the open streams, by tab id (ownStream).
	owners sync.Map
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
	s := &Server{cfg: cfg, db: db, w: w, hub: hub, log: log, commands: edge.NewLimiter(20, 60),
		paceAddrs: edge.NewLimiter(4, 20), paces: edge.NewLimiter(1, 5),
		ackAddrs: edge.NewLimiter(80, 160), acks: edge.NewLimiter(20, 40)}
	var err error
	if s.docs, err = docs.Load(content.Docs, cfg.Dev); err != nil {
		return nil, fmt.Errorf("docs: %w", err)
	}
	if s.history, err = githistory.Load(); err != nil {
		return nil, err
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
			card:   views.Card{Slug: d.Slug, Title: d.Title, Summary: d.Summary, Art: art, Featured: d.Featured},
			server: templ.Raw(server.HTML),
			how:    templ.Raw(how.HTML),
		})
	}
	if err := s.loadStatic(list); err != nil {
		return nil, err
	}
	s.robotsTxt = s.robots()
	if s.sitemapXML, err = s.sitemap(); err != nil {
		return nil, err
	}
	return s, nil
}

// Public returns the routes whose answers are the same for everyone and
// may be cached, by their patterns: the static files, the icons, the
// social image, robots.txt and the sitemap. Mount them outside the
// sessions: a session cookie on an answer a shared cache keeps would go
// to everyone it hands the answer to.
func (s *Server) Public() map[string]http.Handler {
	return map[string]http.Handler{
		"GET /static/":              s.static,
		"GET /favicon.ico":          s.file("image/png", iconPNG32),
		"GET /apple-touch-icon.png": s.file("image/png", iconPNG180),
		"GET /og.png":               s.file("image/png", socialPNG),
		"GET /robots.txt":           s.file("text/plain; charset=utf-8", s.robotsTxt),
		"GET /sitemap.xml":          s.file("application/xml; charset=utf-8", s.sitemapXML),
	}
}

// Static is the site's static files, for a readiness check (Warmed).
func (s *Server) Static() *edge.Static { return s.static }

// Routes returns the handlers, without the edge and without Public's.
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
	mux.HandleFunc("POST /pace", s.pace)
	mux.HandleFunc("POST /ack", s.ack)
	deps := demo.Deps{DB: s.db, W: s.w, Hub: s.hub, Log: s.log, Commands: s.commands, Open: s.owns}
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
		Assets: s.assets, Nonce: edge.Nonce(r.Context()), Version: s.cfg.Version, Repo: s.cfg.Repo, Build: s.hub.Build(),
		Components: s.components(),
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

const description = "lith is a runtime for Go, SQLite and Datastar apps, built by zwei und eins gmbh: one writer, views rendered once per tick for all, and HTML streamed over SSE."

// home is the landing page. Its slots show the demos' embeds, drawn from
// one read transaction and kept current by the page's stream.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sh := s.shell(r, "", description, "/", "")
	sh.JSONLD = s.graph(s.software())
	viewer := demo.Viewer{Session: edge.SessionID(ctx), Tab: demo.NewID()}
	if s.embeds(viewer) != nil {
		sh.Stream, sh.Tab = "/stream", viewer.Tab
	}
	sh.Components = s.homeComponents()
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

// homeComponents are the bundles of the components the landing page
// renders: the quick start's tabs and the embeds'.
func (s *Server) homeComponents() []views.Script {
	uses := []string{"tabs"}
	for _, d := range s.demos {
		if d.Embed != nil {
			uses = append(uses, d.Components...)
		}
	}
	return s.components(uses...)
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
		w.WriteHeader(http.StatusBadRequest) // no page of ours sends it
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
	const about = "Live demos of lith, from a million checkboxes to a shared pixel canvas: each runs on this server, with its code and how it works beside it."
	sh := s.shell(r, "Demos", about, "/demos", "demos")
	sh.JSONLD = s.graph(s.webPage("/demos", "Demos", about))
	s.page(w, r, sh, func(context.Context, *sql.Tx) (templ.Component, error) {
		return views.DemoIndex(s.cards()), nil
	})
}

func (s *Server) cards() []views.Card {
	cards := make([]views.Card, len(s.demos))
	for i, d := range s.demos {
		cards[i] = d.card
	}
	slices.SortStableFunc(cards, func(a, b views.Card) int {
		switch {
		case a.Featured == b.Featured:
			return 0
		case a.Featured:
			return -1
		}
		return 1
	})
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
	sh.JSONLD = s.graph(s.webPage("/demos/"+d.Slug, d.Title, d.Summary))
	sh.Stream, sh.Tab = "/demos/"+d.Slug+"/stream", viewer.Tab
	sh.Components = s.components(d.Components...)
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
		w.WriteHeader(http.StatusNotFound) // no page of ours opens it
		return
	}
	viewer, ok := demo.Who(r, r.URL.Query().Get("tab"))
	if !ok {
		w.WriteHeader(http.StatusBadRequest) // no page of ours sends it
		return
	}
	s.stream(w, r, viewer, d.Page.Views(viewer))
}

// pageState is what a stream's request reports in its signals, for a
// refresh after a deploy (views.openStream): the build and the assets
// version that rendered the page.
type pageState struct {
	Build  string `json:"_build"`
	Assets string `json:"_assets"`
}

// stream serves the tab's stream of views until the tab leaves or the
// server stops; then its last event re-creates the page's stream element,
// which opens it again on the next process. A tab on a slow link gets its
// views' frames about four times a second. The stream counts under its
// client's address, so that one address cannot hold every stream
// (live.Options.MaxStreamsPerClient).
func (s *Server) stream(w http.ResponseWriter, r *http.Request, v demo.Viewer, vs []live.View) {
	var page pageState
	if sse.ReadSignals(w, r, &page) != nil {
		w.WriteHeader(http.StatusBadRequest) // no page of ours sends it
		return
	}
	spec := live.Spec{ID: v.Tab, Client: edge.ClientKey(r), Views: vs, Reconnect: views.StreamID, WriteTick: frameWriter(), Slow: edge.SlowLink(r),
		Build: page.Build, Page: r.Header.Get("Page-Path"), Refresh: func(ctx context.Context, rq live.RefreshRequest) ([][]byte, error) {
			return s.refresh(ctx, rq, page.Assets)
		}}
	s.paceByAcks(&spec)
	defer s.ownStream(v)()
	err := s.hub.Serve(w, r, spec)
	if err != nil && !errors.Is(err, live.ErrFull) && !errors.Is(err, live.ErrClientFull) && !errors.Is(err, live.ErrStopped) && !errors.Is(err, sse.ErrNoRoom) {
		s.log.Debug("stream ended", "tab", v.Tab, "err", err)
	}
}

// frameWriter writes a tick's frames to a stream in one write: a []byte
// as it is, and a demo.Frame's First the first time the stream sends its
// view, its Next after that. A demo.Probe stays with the stream and adds
// its report to the writes from then on (demo.Probes).
func frameWriter() func(*sse.Stream, []live.Frame) error {
	seen := map[string]bool{}
	var events [][]byte
	var probes demo.Probes
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
			case demo.Probe:
				probes.Keep(f.Key, v)
			}
		}
		return probes.Write(st, events)
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
	sh := s.shell(r, title, p.Description, p.Path, "docs")
	sh.Headline = p.Headline
	sh.JSONLD = s.graph(s.article(d, p)...)
	sh.Docs = views.DocsNav(d, p, false)
	s.page(w, r, sh, func(context.Context, *sql.Tx) (templ.Component, error) {
		page := views.DocsPage{Docs: d, Page: p, Edit: s.cfg.Repo + "/blob/main/site/content/docs/" + p.File}
		if c, ok := s.history[p.Path]; ok {
			page.Updated, page.Commit = &c, s.cfg.Repo+"/commit/"+c.Hash
		}
		return views.Docs(page), nil
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
	ctx := demo.WithHub(r.Context(), s.hub) // for demo.Watching in a Part's Render
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
	h.Set("Accept-CH", edge.NetworkHints) // for the streams: edge.SlowLink
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
