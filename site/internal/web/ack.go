package web

import (
	"html"
	"net/http"
	"strconv"
	"time"

	"github.com/zweiundeins/lith/edge"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/views"
)

// paceByAcks has a tab's stream paced by its page's acknowledgements as
// Config.Pacing says: by default the stream starts unpaced, /pace paces it
// once newest-frame.js reports the page behind, and live.Spec.Release ends
// the pacing once the page keeps up again. Each paced write ends with an
// element whose data-init posts /ack for it after the paint that shows it,
// or once applied with Config.AckOnApply, with the page's round trip.
func (s *Server) paceByAcks(spec *live.Spec) {
	window := max(s.cfg.AckWindow, 1)
	switch s.cfg.Pacing {
	case "off":
		return
	case "every":
		spec.Window = window
	case "reopen":
		if spec.Slow {
			spec.Slow, spec.Window = false, window
		}
	}
	tab, paint, rtt := spec.ID, !s.cfg.AckOnApply, s.cfg.AckReleaseSlack >= 0
	spec.Release, spec.ReleaseSlack = s.cfg.AckRelease, s.cfg.AckReleaseSlack
	spec.Ack = func(token string, n int64) []byte {
		num := strconv.FormatInt(n, 10)
		url := "'/ack?tab=" + tab + "&s=" + token + "&n=" + num + "'"
		if rtt {
			url += " + '&rtt=' + " + lastRoundTrip
		}
		post := "@post(" + url + ", {requestCancellation: 'disabled'})"
		if paint {
			post = "requestAnimationFrame(() => setTimeout(() => " + post + "))"
		}
		el := `<i id="ack-` + num + `" data-init="` + html.EscapeString(post) + `"></i>`
		return sse.PatchElements([]byte(el), sse.Patch{Selector: "#" + views.StreamID, Mode: sse.Inner})
	}
}

// lastRoundTrip is the round trip of the page's last /ack or /pace in ms,
// from its Resource Timing, or 0. It clears the entries, so that the
// buffer of 250 never fills with acknowledgements.
const lastRoundTrip = `((x) => (performance.clearResourceTimings(), x ? Math.round(x.responseStart - x.requestStart) : 0))(performance.getEntriesByType('resource').findLast(r => r.name.includes('/ack?') || r.name.includes('/pace?')))`

// ownStream notes that v's session has the stream of v's tab open, until
// the returned function runs. The site names a stream by its tab id alone,
// as its demos' Watch calls do, so /ack and /pace check the session here.
func (s *Server) ownStream(v demo.Viewer) func() {
	owner := &v.Session
	s.owners.Store(v.Tab, owner)
	return func() { s.owners.CompareAndDelete(v.Tab, owner) }
}

// owns reports whether v's session has the stream of v's tab open.
func (s *Server) owns(v demo.Viewer) bool {
	o, ok := s.owners.Load(v.Tab)
	return ok && *o.(*string) == v.Session
}

// pace paces the tab's stream once its page reports that it fell behind
// (newest-frame-behind, live.Hub.Pace), and counts the report in
// lith_site_live_newest_frame_behind_total (live.Hub.Behind). It reaches
// only the request's session's streams, and answers 204, also to an
// address or a session over its limit, whose report it drops.
func (s *Server) pace(w http.ResponseWriter, r *http.Request) {
	v, ok := demo.Who(r, r.URL.Query().Get("tab"))
	if !ok {
		http.Error(w, "bad tab id", http.StatusBadRequest)
		return
	}
	now := time.Now()
	if s.paceAddrs.Allow(edge.ClientKey(r), 1, now) && s.paces.Allow(v.Session, 1, now) && s.owns(v) {
		s.hub.Behind(v.Tab)
		s.hub.Pace(v.Tab, max(s.cfg.AckWindow, 1))
	}
	w.WriteHeader(http.StatusNoContent)
}

// ack takes a page's acknowledgement of its stream's write n
// (live.Hub.Ack), and the round trip it reports (live.Hub.RoundTrip). It
// reaches only the request's session's streams, and answers 204, also to
// an address or a stream over its limit, whose acknowledgement it drops:
// the stream then sends its next write after live.AckTimeout.
func (s *Server) ack(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v, ok := demo.Who(r, q.Get("tab"))
	token := q.Get("s")
	n, err := strconv.ParseInt(q.Get("n"), 10, 64)
	rtt, rttErr := strconv.Atoi(q.Get("rtt"))
	if !ok || !validToken(token) || err != nil || n < 1 || q.Has("rtt") && rttErr != nil {
		http.Error(w, "bad acknowledgement", http.StatusBadRequest)
		return
	}
	now := time.Now()
	if s.ackAddrs.Allow(edge.ClientKey(r), 1, now) && s.acks.Allow(v.Session+"."+v.Tab, 1, now) && s.owns(v) {
		if rtt > 0 && rtt <= 60_000 {
			s.hub.RoundTrip(v.Tab, time.Duration(rtt)*time.Millisecond)
		}
		s.hub.Ack(v.Tab, token, n)
	}
	w.WriteHeader(http.StatusNoContent)
}

// validToken accepts a stream's token (live.Spec.Ack): a number in base 36.
func validToken(s string) bool {
	if len(s) == 0 || len(s) > 13 {
		return false
	}
	for _, c := range []byte(s) {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'z') {
			return false
		}
	}
	return true
}
