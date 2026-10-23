package demo

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/zweiundeins/lith/edge"
	"github.com/zweiundeins/lith/writer"
)

// Ack is what a notice patches into the demo's signals besides its
// problem line: the op id it answers as "ack", and whatever else the
// demo's page needs to end the pending state, such as "accepted": false.
// Nil or empty: the notice carries only its message.
type Ack map[string]any

// overKey marks a request that Action found over its limits.
type overKey struct{}

// Action mounts the action POST /cmd/<slug>/<name>. Before h runs, the
// request has passed the edge's CSRF check, which wants Datastar's
// Datastar-Request header on /cmd/, and the rate limits are checked: the
// site's limit for all actions of a client address (unless rate.Separate),
// then rate per address and per session. h decodes the request first, so
// that the answer to one over them knows its tab and op: Act, or Over,
// answers it with a bare 429 and sends the tab a notice with the op id,
// which ends its pending state. A status carries nothing a person reads.
func (k *Kit) Action(name string, rate Rate, h http.HandlerFunc) {
	if !namePattern.MatchString(name) {
		panic(fmt.Sprintf("demo %s: command name %q: lowercase letters, digits and -", k.slug, name))
	}
	if rate.Every <= 0 || rate.Burst < 1 {
		panic(fmt.Sprintf("demo %s: command %s needs a Rate", k.slug, name))
	}
	perSecond := float64(time.Second) / float64(rate.Every)
	sessions := cmp.Or(max(rate.Sessions, 0), 4)
	byAddr := edge.NewLimiter(float64(sessions)*perSecond, sessions*rate.Burst)
	bySession := edge.NewLimiter(perSecond, rate.Burst)
	k.mux.HandleFunc("POST /cmd/"+k.slug+"/"+name, func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		addr := edge.ClientKey(r)
		ctx := r.Context()
		if (!rate.Separate && !k.Commands.Allow(addr, 1, now)) || !byAddr.Allow(addr, 1, now) || !bySession.Allow(edge.SessionID(ctx), 1, now) {
			ctx = context.WithValue(ctx, overKey{}, true)
		}
		if k.Open != nil {
			ctx = context.WithValue(ctx, openKey{}, k.Open)
		}
		h(w, r.WithContext(ctx))
	})
}

// Limited reports whether Action found the request over its limits, for
// an action whose person has nothing to read about it, such as a pointer's
// move, which answers a bare 429 itself.
func Limited(r *http.Request) bool {
	over, _ := r.Context().Value(overKey{}).(bool)
	return over
}

// Over answers a request that Action found over its limits: a bare 429,
// and a notice to the tab of v with ack. It reports whether it did.
func (k *Kit) Over(w http.ResponseWriter, r *http.Request, v Viewer, ack Ack) bool {
	if !Limited(r) {
		return false
	}
	k.Notify(v, "Too many requests at once. Wait a moment, then try again.", ack)
	w.WriteHeader(http.StatusTooManyRequests)
	return true
}

// Act runs cmd as the action of the tab v and answers: 204 once it has
// committed, whatever became of it (the tab's view shows the outcome).
// What reaches no outcome answers with a status alone, for machines, and
// sends the tab a notice with ack where a person must hear of it: over
// the limits (429), a full queue (503), a failed write (500). A command
// that fails its Validate gets a bare 400: what a person can get wrong
// is a refusal the command records in the tab's state, and only a request
// no page sends fails Validate. A closed writer gets a bare 503: the
// stream has ended, and the reconnected page sees the op unacked.
func (k *Kit) Act(w http.ResponseWriter, r *http.Request, v Viewer, ack Ack, cmd writer.Command) {
	if k.Over(w, r, v, ack) {
		return
	}
	err := k.W.Do(r.Context(), cmd)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, writer.ErrInvalid):
		w.WriteHeader(http.StatusBadRequest)
	case errors.Is(err, writer.ErrClosed):
		w.WriteHeader(http.StatusServiceUnavailable)
	case errors.Is(err, writer.ErrQueueFull):
		k.Notify(v, "The server is busy. Try again in a moment.", ack)
		w.WriteHeader(http.StatusServiceUnavailable)
	default:
		k.Log.Error("action failed", "demo", k.slug, "path", r.URL.Path, "err", err)
		k.Notify(v, "That could not be saved. Try again in a moment.", ack)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// Notify sends the tab of v a notice (live.Hub.Notify): msg in the demo's
// problem line ($_<slug>.problem) and ack merged into its signals. The
// site names a stream by its tab id, and Who lets an action through only
// for a tab whose stream the request's session holds (Deps.Open), so a
// notice reaches that session alone. Each demo has a key of its own, so
// demos that share a stream (the landing page) keep their notices apart.
// It reports whether the stream was open.
func (k *Kit) Notify(v Viewer, msg string, ack Ack) bool {
	if k.Hub == nil || v.Tab == "" {
		return false
	}
	fields := maps.Clone(ack)
	if fields == nil {
		fields = Ack{}
	}
	fields["problem"] = msg
	ev, err := Signals(map[string]any{signalName(k.slug): fields})
	if err != nil {
		k.Log.Error("notice", "demo", k.slug, "err", err)
		return false
	}
	return k.Hub.Notify(v.Tab, k.slug+":notice", ev)
}

// Resync marks every demo's tab view out of sync: the page runs it when it
// opens its stream, when Datastar retries it, and when the tab hides (a
// hidden tab's stream closes, and opens again when it shows). Each demo's
// tab view patches its own <signal>.synced true with every frame, so the
// signal changes only with the first frame after a (re)connect.
const Resync = `@setAll(false, {include: /\.synced$/})`

// Reopen opens the page's stream again, as sse.Reconnect has the page do.
const Reopen = `(s => s.replaceWith(s.cloneNode(true)))(document.getElementById('stream'))`

// Pending describes a demo's pending op on the page, for the expressions
// that end it on a signal and never on a clock: the op's ack (the demo's
// settle), Datastar's retries-failed or a status that comes without a
// notice (Fetched), and the stream connecting again with a tab row that
// does not ack the op, once no request of the op is in flight (Lost). The
// fields are expressions of the demo's signals.
//
// Each demo is one op key: its signal object holds the op, its in-flight
// count and its recheck, so one demo's hung request never holds another's
// lost op. Its requests go out from the element that listens (Match), a
// stage or a form that no frame removes, so the count cannot leak. The
// guide's board sends each request from an element of its own in a
// stable #ops instead, for pages whose forms and menus come and go.
type Pending struct {
	Sig      string // the demo's signal object, such as "_guestbook"
	Match    string // which fetch events are the op's: "evt.detail.el === el"
	Waiting  string // true while an op waits for its ack
	End      string // ends the pending state
	Inflight string // counts the op's requests in flight
	Recheck  string // set when a reconnect came while one was in flight
	Problem  string // the line for the page's own words
	Down     string // the page's words when a request never got through
	Failed   string // the page's words for a status that comes without a notice
	Lost     string // the page's words when the op was lost to a reconnect
}

// NewPending is the Pending of a demo whose signals sig hold op, ack,
// inflight, recheck, synced and problem, with the op ended by clearing it.
func NewPending(sig string) Pending {
	s := "$" + sig + "."
	return Pending{
		Sig:      sig,
		Match:    "evt.detail.el === el",
		Waiting:  s + "op !== '' && " + s + "ack !== " + s + "op",
		End:      s + "op = ''",
		Inflight: s + "inflight",
		Recheck:  s + "recheck",
		Problem:  s + "problem",
		Down:     "The connection is down. Try again once it is back.",
		Failed:   "That did not work. Try again.",
		Lost:     "That did not go through. Try again.",
	}
}

// Fetched is the data-on:datastar-fetch of the elements whose actions
// carry the op. It counts the op's requests in flight, ends the pending
// state when one never got through (retries-failed) or answered a status
// that comes without a notice, and, when the last one ends after a
// reconnect that found no ack, opens the stream once more, so that a
// fresh frame of the tab's row decides. Act sends a notice with every 429
// and 5xx but a closed writer's 503, whose stream is ending: those leave
// the op to the stream, and so does a 409, a tab without a stream, whose
// reconnect decides. Any other 4xx has none, a 400 for a request
// no page sends, so the page ends the op itself.
func (p Pending) Fetched() string {
	return p.Match + ` && (
	evt.detail.type === 'started' ? (` + p.Inflight + ` = ` + p.Inflight + ` + 1)
	: evt.detail.type === 'finished' ? (` + p.Inflight + ` = Math.max(0, ` + p.Inflight + ` - 1), ` + p.Inflight + ` === 0 && ` + p.Recheck + ` && (` + p.Recheck + ` = false, (` + p.Waiting + `) && ` + Reopen + `))
	: evt.detail.type === 'error' ? (` + p.Waiting + `) && +evt.detail.argsRaw.status !== 409 && +evt.detail.argsRaw.status !== 429 && +evt.detail.argsRaw.status < 500 && (` + p.End + `, ` + p.Problem + ` = ` + jsQuote(p.Failed) + `)
	: evt.detail.type === 'retries-failed' && (` + p.Waiting + `) && (` + p.End + `, ` + p.Problem + ` = ` + jsQuote(p.Down) + `))`
}

// LostExpr is the data-on-signal-patch that ends the op when the stream
// connected again and the demo's tab view's first frame does not ack it.
// While a request is in flight nothing is decided yet: it leaves the op
// to Fetched.
func (p Pending) LostExpr() string {
	return `patch.` + p.Sig + `?.synced && (` + p.Waiting + `) && (` + p.Inflight + ` > 0 ? (` + p.Recheck + ` = true) : (` + p.End + `, ` + p.Problem + ` = ` + jsQuote(p.Lost) + `))`
}

// LostFilter is the data-on-signal-patch-filter that keeps LostExpr to
// the changes of the demo's synced signal: a click alone never runs it.
func (p Pending) LostFilter() string {
	return `{include: /^` + regexp.QuoteMeta(p.Sig) + `\.synced$/}`
}

func jsQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}
