package edge

import (
	"net/http"
	"net/netip"
	"strings"
)

// ClientIP returns the address of the client that sent r. A peer on loopback
// or a Unix socket is the local reverse proxy, which names the client as the
// last address of X-Forwarded-For. The proxy must set that header: Caddy's
// reverse_proxy does by default, nginx with proxy_set_header X-Forwarded-For
// $proxy_add_x_forwarded_for. X-Real-IP counts only when there is no
// X-Forwarded-For: a proxy may pass on an X-Real-IP the client sent, as Caddy
// does unless the site block sets it. From any other peer both headers are
// ignored: a client can send them itself, to dodge its rate limit or to spend
// someone else's. When the proxy names no address that parses, the address is
// the peer's, invalid for a Unix socket.
func ClientIP(r *http.Request) netip.Addr {
	peer := parseAddr(r.RemoteAddr)
	if peer.IsValid() && !peer.IsLoopback() {
		return peer
	}
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		last := xff[len(xff)-1]
		if i := strings.LastIndexByte(last, ','); i >= 0 {
			last = last[i+1:]
		}
		if ip := parseAddr(strings.TrimSpace(last)); ip.IsValid() {
			return ip
		}
		return peer
	}
	if ip := parseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip.IsValid() {
		return ip
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
