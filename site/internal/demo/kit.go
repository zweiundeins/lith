package demo

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/edge"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"
)

// Deps are what the site hands every demo's kit.
type Deps struct {
	DB  *sqlite.DB
	W   *writer.Writer
	Hub *live.Hub
	Log *slog.Logger
	// Commands limits all actions of the site together, per client
	// address, before a command's own Rate.
	Commands *edge.Limiter
	// Open reports whether the viewer's session has the stream of the
	// viewer's tab open. Who, in an action that Command mounted, refuses a
	// tab without one: its state would be a row for a tab id the client
	// made up, which no page shows. Nil: Who checks the id alone.
	Open func(Viewer) bool
}

// Kit is what a demo's Mount gets: the database, the writer and the hub,
// and the methods that mount its routes.
type Kit struct {
	Deps
	slug string
	mux  *http.ServeMux
}

// NewKit returns the kit of the demo with the slug, mounting on mux. The
// site makes one per demo; tests make their own.
func NewKit(slug string, mux *http.ServeMux, deps Deps) *Kit {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	if deps.Commands == nil {
		deps.Commands = edge.NewLimiter(20, 60)
	}
	return &Kit{Deps: deps, slug: slug, mux: mux}
}

// Rate limits one command per session: Burst at once, then one every
// Every. A client address gets Sessions times as much, four by default,
// since several people can share one (an office, a mobile network), and
// its limit comes first: a client mints a session with every request that
// sends no cookie, so a session id is no limiter key on its own.
type Rate struct {
	Every time.Duration
	Burst int
	// Separate keeps the command out of the site's limit for all actions
	// of an address. Set it for requests a page sends many times a second
	// (a pointer's moves, a brush's strokes), which would use that limit up
	// for every other action; the command's own limits still hold.
	Separate bool
	// Sessions is how many sessions' worth of the command an address may
	// send (default 4). A command that costs a render of its own and that
	// one person sends many times a second, such as a slider's position,
	// sets 1.
	Sessions int
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// openKey carries Deps.Open to Who in an action.
type openKey struct{}

// Get mounts GET /demos/<slug>/<path> (path without a leading /). The site
// serves /demos/<slug> and /demos/<slug>/stream itself.
func (k *Kit) Get(path string, h http.HandlerFunc) {
	k.mux.HandleFunc("GET /demos/"+k.slug+"/"+strings.TrimPrefix(path, "/"), h)
}

// Fail logs err and answers the action 500.
func (k *Kit) Fail(w http.ResponseWriter, r *http.Request, err error) {
	k.Log.Error("request failed", "demo", k.slug, "method", r.Method, "path", r.URL.Path, "err", err)
	w.WriteHeader(http.StatusInternalServerError)
}

// Decode reads an action's payload, a JSON body of at most 8 KiB, into v.
// When it cannot, or the body is empty, it answers a bare 400 itself and
// returns false: no page of ours sends such a body.
func Decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength == 0 || sse.ReadSignalsLimit(w, r, v, 8<<10) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return false
	}
	return true
}

// Who returns the viewer of an action: the request's session, and the tab
// id its payload carries. It reports false when tab is not an id a page
// makes, and, in an action that Command mounted, when the session has no
// open stream for the tab (Deps.Open).
func Who(r *http.Request, tab string) (Viewer, bool) {
	v := Viewer{Session: edge.SessionID(r.Context()), Tab: tab}
	if !ValidID(tab) {
		return v, false
	}
	if open, ok := r.Context().Value(openKey{}).(func(Viewer) bool); ok && !open(v) {
		return v, false
	}
	return v, true
}

// NewID returns 128 random bits in base64url: a tab id.
func NewID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ValidID accepts tab ids and the op ids pages make: 1 to 64 letters,
// digits, - and _.
func ValidID(s string) bool {
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

// Patch renders c into an event that morphs it into the page by its id,
// or as p says (a selector, a mode, a view transition).
func Patch(ctx context.Context, c templ.Component, p ...sse.Patch) ([]byte, error) {
	var b strings.Builder
	if err := c.Render(ctx, &b); err != nil {
		return nil, err
	}
	return sse.PatchElements([]byte(b.String()), p...), nil
}

// Signals formats v, marshalled to JSON, as an event that merges it into
// the page's signals.
func Signals(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return sse.PatchSignals(b, false), nil
}

// Querier returns what a query should run on: in a view's Render, the
// worker's prepared statements in the render's snapshot
// (sqlite.QuerierFrom); elsewhere tx itself.
func Querier(ctx context.Context, tx *sql.Tx) sqlite.Querier {
	if q, ok := sqlite.QuerierFrom(ctx); ok {
		return q
	}
	return tx
}

// Frame is a view's frame in two forms: a stream sends First as the view's
// first frame and Next from then on. A view whose patch animates
// (sse.Patch{ViewTransition: true}) renders First without the transition:
// the page already shows what the first frame holds, and a view transition
// started then would cut short the one that brought the reader to the page.
type Frame struct {
	First, Next []byte
}

// Animated renders c into a Frame whose later frames morph inside a view
// transition, p's ViewTransition set; the first frame morphs at once.
// p.ViewTransitionSelector is required: it scopes the transition to that
// element (usually the one the frame patches), so the rest of the page
// keeps taking clicks. A transition of the whole document gives every
// press on the page to <html> while it runs. Browsers without scoped
// transitions morph at once.
func Animated(ctx context.Context, c templ.Component, p sse.Patch) (Frame, error) {
	if p.ViewTransitionSelector == "" {
		return Frame{}, errors.New("demo.Animated: set the patch's ViewTransitionSelector to the element that holds what moves")
	}
	var b strings.Builder
	if err := c.Render(ctx, &b); err != nil {
		return Frame{}, err
	}
	html := []byte(b.String())
	plain := p
	plain.ViewTransition, plain.ViewTransitionSelector = false, ""
	p.ViewTransition = true
	return Frame{First: sse.PatchElements(html, plain), Next: sse.PatchElements(html, p)}, nil
}
