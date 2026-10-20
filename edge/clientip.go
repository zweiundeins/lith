package edge

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
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

// TrustedProxies says, for a log line at start, whose X-Forwarded-For and
// X-Real-IP ClientIP believes on a server that listens at addr (a
// listener's Addr). ClientIP believes peers on loopback and on a Unix
// socket: every local user who can connect to them can name any client
// address, and so dodge a per-address limit or spend someone else's. A
// socket whose mode lets only the proxy's group in (0660, the app's group
// holding the proxy's user) narrows that to the proxy. A proxy on another
// host, or on a container bridge, is not believed: every client then
// counts as the proxy's address, and one abuser spends the limit of all.
func TrustedProxies(addr net.Addr) string {
	switch a := addr.(type) {
	case *net.UnixAddr:
		if fi, err := os.Stat(a.Name); err == nil {
			return fmt.Sprintf("whoever may connect to the Unix socket %s (mode %04o)", a.Name, fi.Mode().Perm())
		}
		return "whoever may connect to the Unix socket " + a.Name
	case *net.TCPAddr:
		if a.IP.IsLoopback() {
			return "every local user: peers on loopback, where the proxy connects from"
		}
		return "peers on loopback only: a proxy that connects from another address counts as one client for all"
	}
	return "peers on loopback and on Unix sockets"
}
