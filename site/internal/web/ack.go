package web

import (
	"html"
	"net/http"
	"strconv"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"

	"github.com/zweiundeins/lith/site/internal/views"
)

// paceByAcks paces a tab's stream by the page's acknowledgements when the
// site is configured for it (Config.AckWindow), an experiment beside
// newest-frame.js: each write of frames ends with an element whose
// data-init posts /ack for it, once Datastar has applied the write, or
// after the next paint with Config.AckAfterPaint.
//
// With Config.Hybrid, a stream starts unpaced: "reopen" paces a stream
// that the page opened again with Save-Data: on once it fell behind, in
// place of the slow stream it would get, and "inplace" paces the stream
// the page has open once the page posts /pace.
func (s *Server) paceByAcks(spec *live.Spec) {
	if s.cfg.AckWindow <= 0 {
		return
	}
	tab, paint := spec.ID, s.cfg.AckAfterPaint
	switch s.cfg.Hybrid {
	case "reopen":
		if spec.Slow {
			spec.Slow, spec.Window = false, s.cfg.AckWindow
		}
	case "inplace":
	default:
		spec.Window = s.cfg.AckWindow
	}
	spec.Release, spec.ReleaseSlack = s.cfg.AckRelease, s.cfg.AckReleaseSlack
	spec.Ack = func(token string, n int64) []byte {
		num := strconv.FormatInt(n, 10)
		url := "'/ack?tab=" + tab + "&s=" + token + "&n=" + num + "'"
		if s.cfg.AckReleaseSlack >= 0 {
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

// ack takes a page's acknowledgement of its stream's writes (live.Hub.Ack),
// and the round trip it reports (live.Hub.RoundTrip).
func (s *Server) ack(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	n, err := strconv.ParseInt(q.Get("n"), 10, 64)
	if err != nil {
		http.Error(w, "bad ack", http.StatusBadRequest)
		return
	}
	if ms, err := strconv.Atoi(q.Get("rtt")); err == nil && ms > 0 {
		s.hub.RoundTrip(q.Get("tab"), time.Duration(ms)*time.Millisecond)
	}
	s.hub.Ack(q.Get("tab"), q.Get("s"), n)
	w.WriteHeader(http.StatusNoContent)
}

// pace paces the tab's stream once its page reports that it is behind
// (Config.Hybrid "inplace", live.Hub.Pace), and counts the report as
// /behind does.
func (s *Server) pace(w http.ResponseWriter, r *http.Request) {
	s.hub.Behind(r.URL.Query().Get("tab"))
	s.hub.Pace(r.URL.Query().Get("tab"), s.cfg.AckWindow)
	w.WriteHeader(http.StatusNoContent)
}
