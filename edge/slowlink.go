package edge

import (
	"net/http"
	"strconv"
	"strings"
)

// NetworkHints are the client hints SlowLink reads besides Save-Data, for
// an Accept-CH header on documents: Chromium then sends them with the
// requests that follow, the page's stream among them. Other browsers send
// none, and the page's own measurement (net=slow) stands in for them.
const NetworkHints = "ECT, RTT, Downlink"

// The thresholds of a slow link: a round trip over 300 ms or less than
// 100 KB/s, just past where navigator.connection's effective type turns
// from 4g to 3g (270 ms, 700 kbit/s).
const (
	slowRTT      = 300 // ms
	slowDownlink = 0.8 // Mbit/s
)

// SlowLink reports whether r says its client is on a slow link. Any of
// these does: the query parameter net=slow, which a page adds to its
// stream's URL from its own measurement (navigator.connection's saveData
// or an effective type of 2g or 3g, or where that is missing, a document
// that took over 300 ms to its first byte or came at under 100 KB/s); the
// header Save-Data: on; Chromium's client hints, after an Accept-CH of
// NetworkHints: ECT slow-2g, 2g or 3g, RTT over 300 (ms), Downlink under
// 0.8 (Mbit/s). A client that claims a slow link without one gets fewer
// frames and an encoder that costs the server more memory, within the
// budget that guards it (live.Spec.Slow).
func SlowLink(r *http.Request) bool {
	if r.URL.Query().Get("net") == "slow" {
		return true
	}
	h := r.Header
	if strings.EqualFold(strings.TrimSpace(h.Get("Save-Data")), "on") {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(h.Get("ECT"))) {
	case "slow-2g", "2g", "3g":
		return true
	}
	if rtt, err := strconv.Atoi(strings.TrimSpace(h.Get("RTT"))); err == nil && rtt > slowRTT {
		return true
	}
	if mbps, err := strconv.ParseFloat(strings.TrimSpace(h.Get("Downlink")), 64); err == nil && mbps > 0 && mbps < slowDownlink {
		return true
	}
	return false
}
