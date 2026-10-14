package demo

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
// Every. A client address gets four times as much, since several people
// can share one (an office, a mobile network).
type Rate struct {
	Every time.Duration
	Burst int
	// Separate keeps the command out of the site's limit for all actions
	// of an address. Set it for requests a page sends many times a second
	// (a pointer's moves, a brush's strokes), which would use that limit up
	// for every other action; the command's own limits still hold.
	Separate bool
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Command mounts the action POST /cmd/<slug>/<name>. Before h runs, the
// request has passed the edge's CSRF check, which wants Datastar's
// Datastar-Request header on /cmd/, and the rate limits: the site's limit
// for all actions of a client address (unless rate.Separate), then rate per
// address and per session. A request over a limit gets 429, which the page
// reports like any failed action.
func (k *Kit) Command(name string, rate Rate, h http.HandlerFunc) {
	if !namePattern.MatchString(name) {
		panic(fmt.Sprintf("demo %s: command name %q: lowercase letters, digits and -", k.slug, name))
	}
	if rate.Every <= 0 || rate.Burst < 1 {
		panic(fmt.Sprintf("demo %s: command %s needs a Rate", k.slug, name))
	}
	perSecond := float64(time.Second) / float64(rate.Every)
	byAddr := edge.NewLimiter(4*perSecond, 4*rate.Burst)
	bySession := edge.NewLimiter(perSecond, rate.Burst)
	k.mux.HandleFunc("POST /cmd/"+k.slug+"/"+name, func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		addr := edge.ClientKey(r)
		if (!rate.Separate && !k.Commands.Allow(addr, 1, now)) || !byAddr.Allow(addr, 1, now) || !bySession.Allow(edge.SessionID(r.Context()), 1, now) {
			http.Error(w, "Too many requests at once. Wait a moment.", http.StatusTooManyRequests)
			return
		}
		h(w, r)
	})
}

// Get mounts GET /demos/<slug>/<path> (path without a leading /). The site
// serves /demos/<slug> and /demos/<slug>/stream itself.
func (k *Kit) Get(path string, h http.HandlerFunc) {
	k.mux.HandleFunc("GET /demos/"+k.slug+"/"+strings.TrimPrefix(path, "/"), h)
}

// Do runs cmd on the writer and answers the action: 204 once it has
// committed, 400 with its message when it fails Validate, 503 while the
// server shuts down, and 500 for anything else, which it logs. The page
// learns the outcome from its stream, never from this answer.
func (k *Kit) Do(w http.ResponseWriter, r *http.Request, cmd writer.Command) {
	err := k.W.Do(r.Context(), cmd)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, writer.ErrInvalid):
		http.Error(w, strings.TrimPrefix(err.Error(), writer.ErrInvalid.Error()+": "), http.StatusBadRequest)
	case errors.Is(err, writer.ErrClosed), errors.Is(err, writer.ErrQueueFull):
		http.Error(w, "The server is restarting. Try again in a moment.", http.StatusServiceUnavailable)
	default:
		k.Fail(w, r, err)
	}
}

// Fail logs err and answers 500.
func (k *Kit) Fail(w http.ResponseWriter, r *http.Request, err error) {
	k.Log.Error("request failed", "demo", k.slug, "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, "Something went wrong.", http.StatusInternalServerError)
}

// Decode reads an action's payload, a JSON body of at most 8 KiB, into v.
// When it cannot, or the body is empty, it answers 400 itself and returns
// false.
func Decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength == 0 || sse.ReadSignalsLimit(w, r, v, 8<<10) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

// Who returns the viewer of an action: the request's session, and the tab
// id its payload carries. It reports false when tab is not an id a page
// makes.
func Who(r *http.Request, tab string) (Viewer, bool) {
	return Viewer{Session: edge.SessionID(r.Context()), Tab: tab}, ValidID(tab)
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
