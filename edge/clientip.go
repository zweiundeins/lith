package edge

import (
	"context"
	"errors"
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
// the peer's, invalid for a Unix socket. Behind proxies on other addresses,
// BehindProxies resolves the address, and ClientIP returns what it found.
func ClientIP(r *http.Request) netip.Addr {
	if ip, ok := r.Context().Value(clientIPKey{}).(netip.Addr); ok {
		return ip
	}
	ip, _ := clientIP(r, nil)
	return ip
}

type clientIPKey struct{}

// BehindProxies believes X-Forwarded-For and X-Real-IP from peers in
// proxies too, as ClientIP does from loopback and Unix sockets, for a proxy
// on another host or a container bridge. It goes around the server's
// handler, before anything that reads ClientIP or ClientKey. For a request
// from such a peer, or from loopback or a Unix socket, it walks
// X-Forwarded-For from its end, past every address inside proxies, and the
// client is the first address outside them: each proxy of a chain appends
// the address it got the request from. An address that does not parse ends
// the walk, and so does the header's start: the client is then the last
// address the walk reached, the peer if none. X-Real-IP counts only without
// X-Forwarded-For. A request from any other peer passes untouched, and
// ClientIP ignores its headers. Every address in proxies can name any
// client, to dodge a limit or to spend someone else's: list your own
// proxies only. With no proxies, BehindProxies returns next.
func BehindProxies(proxies []netip.Prefix, next http.Handler) http.Handler {
	if len(proxies) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip, ok := clientIP(r, proxies); ok {
			r = r.WithContext(context.WithValue(r.Context(), clientIPKey{}, ip))
		}
		next.ServeHTTP(w, r)
	})
}

// ParseProxies reads a comma-separated list of addresses and prefixes, such
// as "10.0.0.0/8, 192.0.2.7, fd00::/8", for BehindProxies. A bare address is
// a /32 or a /128, an IPv4-mapped one an IPv4 address. An empty list is
// none.
func ParseProxies(list string) ([]netip.Prefix, error) {
	if strings.TrimSpace(list) == "" {
		return nil, nil
	}
	var proxies []netip.Prefix
	var errs []error
	for s := range strings.SplitSeq(list, ",") {
		s = strings.TrimSpace(s)
		if !strings.Contains(s, "/") {
			a, err := netip.ParseAddr(s)
			if err != nil {
				errs = append(errs, fmt.Errorf("trusted proxy %q: %w", s, err))
				continue
			}
			a = a.Unmap().WithZone("")
			proxies = append(proxies, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("trusted proxy %q: %w", s, err))
			continue
		}
		if a := p.Addr(); a.Is4In6() {
			if p.Bits() < 96 {
				errs = append(errs, fmt.Errorf("trusted proxy %q: an IPv4-mapped prefix shorter than /96", s))
				continue
			}
			p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
		}
		proxies = append(proxies, p.Masked())
	}
	return proxies, errors.Join(errs...)
}

// clientIP walks r's proxy headers when its peer is trusted: on loopback,
// a Unix socket, or in proxies. It reports whether the peer was.
func clientIP(r *http.Request, proxies []netip.Prefix) (netip.Addr, bool) {
	peer := parseAddr(r.RemoteAddr)
	if peer.IsValid() && !peer.IsLoopback() && !inside(peer, proxies) {
		return peer, false
	}
	xff := r.Header.Values("X-Forwarded-For")
	if len(xff) == 0 {
		if ip := parseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip.IsValid() {
			return ip, true
		}
		return peer, true
	}
	ip := peer
	for i := len(xff) - 1; i >= 0; i-- {
		line := xff[i]
		for {
			j := strings.LastIndexByte(line, ',')
			a := parseAddr(strings.TrimSpace(line[j+1:]))
			if !a.IsValid() {
				return ip, true
			}
			ip = a
			if !inside(a, proxies) {
				return ip, true
			}
			if j < 0 {
				break
			}
			line = line[:j]
		}
	}
	return ip, true
}

func inside(a netip.Addr, proxies []netip.Prefix) bool {
	for _, p := range proxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
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
// listener's Addr), with proxies the prefixes given to BehindProxies.
// ClientIP believes peers on loopback and on a Unix socket: every local
// user who can connect to them can name any client address, and so dodge
// a per-address limit or spend someone else's. A socket whose mode lets
// only the proxy's group in (0660, the app's group holding the proxy's
// user) narrows that to the proxy. A proxy on another host, or on a
// container bridge, is believed only inside proxies: without them, every
// client counts as the proxy's address, and one abuser spends the limit
// of all.
func TrustedProxies(addr net.Addr, proxies ...netip.Prefix) string {
	var s string
	switch a := addr.(type) {
	case *net.UnixAddr:
		if fi, err := os.Stat(a.Name); err == nil {
			s = fmt.Sprintf("whoever may connect to the Unix socket %s (mode %04o)", a.Name, fi.Mode().Perm())
		} else {
			s = "whoever may connect to the Unix socket " + a.Name
		}
	case *net.TCPAddr:
		switch {
		case a.IP.IsLoopback():
			s = "every local user: peers on loopback, where the proxy connects from"
		case len(proxies) == 0:
			s = "peers on loopback only: a proxy that connects from another address counts as one client for all"
		default:
			s = "peers on loopback"
		}
	default:
		s = "peers on loopback and on Unix sockets"
	}
	if len(proxies) > 0 {
		names := make([]string, len(proxies))
		for i, p := range proxies {
			names[i] = p.String()
		}
		s += "; and peers in " + strings.Join(names, ", ") + ", each of which can name any client"
	}
	return s
}
