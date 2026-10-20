package web

import (
	"html"
	"net/http"
	"strconv"
	"time"

	"github.com/zweiundeins/lith/edge"
	"github.com/zweiundeins/lith/sse"
)

// streamID names a tab's stream after its session too, so that /ack and
// /pace reach only the streams of the request's session.
func streamID(sid, tab string) string {
	return sid + "." + tab
}

// ackEvent is the tab's live.Spec.Ack: once /pace has paced the stream,
// each write ends with an element in the stream's element whose data-init
// posts /ack for the write after the paint that shows it, with the round
// trip of the page's last /ack or /pace.
func ackEvent(tab string) func(token string, n int64) []byte {
	return func(token string, n int64) []byte {
		num := strconv.FormatInt(n, 10)
		post := "@post('/ack?tab=" + tab + "&s=" + token + "&n=" + num + "&rtt=' + " + lastRoundTrip + ", {requestCancellation: 'disabled'})"
		el := `<i id="ack-` + num + `" data-init="` + html.EscapeString("requestAnimationFrame(() => setTimeout(() => "+post+"))") + `"></i>`
		return sse.PatchElements([]byte(el), sse.Patch{Selector: "#stream", Mode: sse.Inner})
	}
}

// lastRoundTrip is the round trip of the page's last /ack or /pace in ms,
// from its Resource Timing, or 0. It clears the entries, so that the
// buffer of 250 never fills with acknowledgements.
const lastRoundTrip = `((x) => (performance.clearResourceTimings(), x ? Math.round(x.responseStart - x.requestStart) : 0))(performance.getEntriesByType('resource').findLast(r => r.name.includes('/ack?') || r.name.includes('/pace?')))`

// pace paces the tab's stream once its page reports that it fell behind
// (newest-frame-behind): the hub then sends the page a write only once it
// has acknowledged the last (live.Hub.Pace), and sends every frame again
// once it keeps up. It counts the report in
// app_live_newest_frame_behind_total (live.Hub.Behind). It answers 204,
// also to an address or a session over its limit, whose report it drops.
func (s *Server) pace(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	if !validID(tab) {
		http.Error(w, "bad tab id", http.StatusBadRequest)
		return
	}
	sid, now := edge.SessionID(r.Context()), time.Now()
	if s.paceAddrs.Allow(edge.ClientKey(r), 1, now) && s.paces.Allow(sid, 1, now) {
		id := streamID(sid, tab)
		s.hub.Behind(id)
		s.hub.Pace(id, 1)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ack takes the page's acknowledgement of its paced stream's write n
// (live.Hub.Ack), and the round trip it reports (live.Hub.RoundTrip). It
// answers 204, also to an address or a stream over its limit, whose
// acknowledgement it drops: the stream then sends its next write after
// live.AckTimeout. The second limit is per stream, so a session with
// several paced tabs does not share it.
func (s *Server) ack(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tab, token := q.Get("tab"), q.Get("s")
	n, err := strconv.ParseInt(q.Get("n"), 10, 64)
	rtt, rttErr := strconv.Atoi(q.Get("rtt"))
	if !validID(tab) || !validToken(token) || err != nil || n < 1 || q.Has("rtt") && rttErr != nil {
		http.Error(w, "bad acknowledgement", http.StatusBadRequest)
		return
	}
	id, now := streamID(edge.SessionID(r.Context()), tab), time.Now()
	if s.ackAddrs.Allow(edge.ClientKey(r), 1, now) && s.acks.Allow(id, 1, now) {
		if rtt > 0 && rtt <= 60_000 {
			s.hub.RoundTrip(id, time.Duration(rtt)*time.Millisecond)
		}
		s.hub.Ack(id, token, n)
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
