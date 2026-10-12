package edge

import (
	"net/http"
	"net/netip"
	"strings"
)

// ClientIP returns the address of the client that sent r. The local reverse
// proxy reaches the app over loopback or a Unix socket and names the client in
// X-Real-IP, or as the last address of X-Forwarded-For. From any other peer
// those headers are ignored: a client can send them itself, to dodge its rate
// limit or to spend someone else's. The address is invalid when the peer is a
// Unix socket and the proxy named no one.
func ClientIP(r *http.Request) netip.Addr {
	peer := parseAddr(r.RemoteAddr)
	if peer.IsValid() && !peer.IsLoopback() {
		return peer
	}
	if ip := parseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip.IsValid() {
		return ip
	}
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		last := xff[len(xff)-1]
		if i := strings.LastIndexByte(last, ','); i >= 0 {
			last = last[i+1:]
		}
		if ip := parseAddr(strings.TrimSpace(last)); ip.IsValid() {
			return ip
		}
	}
	return peer
}

// ClientKey is ClientIP as a rate-limit key. An IPv6 client counts by its /64,
// since one subscriber holds a whole /64 and could rotate through it; an
// unknown address is "local".
func ClientKey(r *http.Request) string {
	ip := ClientIP(r)
	switch {
	case !ip.IsValid():
		return "local"
	case ip.Is6():
		p, _ := ip.Prefix(64)
		return p.String()
	}
	return ip.String()
}

// parseAddr reads "ip:port" or a bare ip, IPv4-mapped addresses as IPv4.
func parseAddr(s string) netip.Addr {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap().WithZone("")
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap().WithZone("")
	}
	return netip.Addr{}
}
