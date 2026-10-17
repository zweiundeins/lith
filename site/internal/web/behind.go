package web

import (
	"net/http"
	"time"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// reportBehind counts a page's report that it fell behind its stream
// (newest-frame-behind) in lith_site_live_newest_frame_behind_total,
// through live.Hub.Behind, which counts only for an open stream and once
// every 2 s at most. The tab id is the check that the stream is the
// page's own: it is random, and only that tab knows it. A session over
// its limit gets 204 too, and its report is dropped.
func (s *Server) reportBehind(w http.ResponseWriter, r *http.Request) {
	v, ok := demo.Who(r, r.URL.Query().Get("tab"))
	if !ok {
		http.Error(w, "bad tab id", http.StatusBadRequest)
		return
	}
	if s.behind.Allow(v.Session, 1, time.Now()) {
		s.hub.Behind(v.Tab)
	}
	w.WriteHeader(http.StatusNoContent)
}
