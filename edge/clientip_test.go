package edge

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientIPTrustsOnlyTheLocalProxy(t *testing.T) {
	for _, c := range []struct {
		name, remote, realIP string
		xff                  []string
		want, key            string
	}{
		{"direct client", "203.0.113.9:4711", "", nil, "203.0.113.9", "203.0.113.9"},
		{"direct client forging X-Real-IP", "203.0.113.9:4711", "198.51.100.7", nil, "203.0.113.9", "203.0.113.9"},
		{"direct client forging X-Forwarded-For", "203.0.113.9:4711", "", []string{"198.51.100.7"}, "203.0.113.9", "203.0.113.9"},
		{"direct client forging both", "203.0.113.9:4711", "198.51.100.7", []string{"192.0.2.1"}, "203.0.113.9", "203.0.113.9"},
		{"direct IPv6 client forging both", "[2001:db8::5]:4711", "198.51.100.7", []string{"192.0.2.1"}, "2001:db8::5", "2001:db8::/64"},
		{"LAN peer is not the local proxy", "10.0.0.2:4711", "198.51.100.7", []string{"192.0.2.1"}, "10.0.0.2", "10.0.0.2"},
		{"proxy over loopback", "127.0.0.1:4711", "", []string{"198.51.100.7"}, "198.51.100.7", "198.51.100.7"},
		{"proxy over IPv6 loopback", "[::1]:4711", "", []string{"198.51.100.7"}, "198.51.100.7", "198.51.100.7"},
		{"proxy over a Unix socket", "@", "", []string{"198.51.100.7"}, "198.51.100.7", "198.51.100.7"},
		{"proxy over an unnamed Unix socket", "", "", []string{"198.51.100.7"}, "198.51.100.7", "198.51.100.7"},
		{"X-Real-IP alone", "127.0.0.1:4711", "198.51.100.7", nil, "198.51.100.7", "198.51.100.7"},
		{"X-Real-IP alone over a Unix socket", "@", "198.51.100.7", nil, "198.51.100.7", "198.51.100.7"},
		{"X-Real-IP a client sent, passed on beside the proxy's X-Forwarded-For", "127.0.0.1:4711", "198.51.100.7", []string{"203.0.113.9"}, "203.0.113.9", "203.0.113.9"},
		{"forged X-Real-IP beside X-Forwarded-For over a Unix socket", "@", "198.51.100.7", []string{"203.0.113.9"}, "203.0.113.9", "203.0.113.9"},
		{"chain: the last address is the proxy's peer", "127.0.0.1:4711", "", []string{"198.51.100.7, 192.0.2.1, 203.0.113.9"}, "203.0.113.9", "203.0.113.9"},
		{"chain without spaces, beside a forged X-Real-IP", "127.0.0.1:4711", "198.51.100.7", []string{"192.0.2.1,203.0.113.9"}, "203.0.113.9", "203.0.113.9"},
		{"chain over several header lines", "@", "198.51.100.7", []string{"192.0.2.1", "198.51.100.8, 203.0.113.9"}, "203.0.113.9", "203.0.113.9"},
		{"IPv6 last in the chain", "[::1]:4711", "198.51.100.7", []string{"198.51.100.8, 2001:db8:1:2::9"}, "2001:db8:1:2::9", "2001:db8:1:2::/64"},
		{"IPv4-mapped IPv6 in X-Forwarded-For", "127.0.0.1:4711", "", []string{"::ffff:203.0.113.9"}, "203.0.113.9", "203.0.113.9"},
		{"garbage X-Real-IP beside X-Forwarded-For", "127.0.0.1:4711", "not an ip", []string{"198.51.100.7"}, "198.51.100.7", "198.51.100.7"},
		{"X-Real-IP does not stand in for a garbage X-Forwarded-For", "127.0.0.1:4711", "198.51.100.7", []string{"192.0.2.1, unknown"}, "127.0.0.1", "127.0.0.1"},
		{"X-Real-IP does not stand in for an empty X-Forwarded-For", "@", "198.51.100.7", []string{""}, "invalid IP", "local"},
		{"loopback without headers", "127.0.0.1:4711", "", nil, "127.0.0.1", "127.0.0.1"},
		{"Unix socket without headers", "@", "", nil, "invalid IP", "local"},
		{"IPv4-mapped IPv6", "[::ffff:203.0.113.9]:4711", "", nil, "203.0.113.9", "203.0.113.9"},
		{"IPv6 keyed by its /64", "[2001:db8:1:2:3:4:5:6]:4711", "", nil, "2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		{"IPv6 from the proxy keyed by its /64", "127.0.0.1:4711", "", []string{"2001:db8:1:2:ffff::1"}, "2001:db8:1:2:ffff::1", "2001:db8:1:2::/64"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = c.remote
			if c.realIP != "" {
				r.Header.Set("X-Real-IP", c.realIP)
			}
			for _, v := range c.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := ClientIP(r).String(); got != c.want {
				t.Errorf("ClientIP = %s, want %s", got, c.want)
			}
			if got := ClientKey(r); got != c.key {
				t.Errorf("ClientKey = %s, want %s", got, c.key)
			}
		})
	}
}

// TrustedProxies names whom a listener's proxy headers come from: the
// socket with its mode, every local user on loopback, and on another
// address no one but loopback peers.
func TestTrustedProxies(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "app.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	os.Chmod(sock, 0o660)
	for _, c := range []struct {
		addr net.Addr
		want string
	}{
		{ln.Addr(), "Unix socket " + sock + " (mode 0660)"},
		{&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}, "every local user"},
		{&net.TCPAddr{IP: net.ParseIP("::1"), Port: 8080}, "every local user"},
		{&net.TCPAddr{IP: net.IPv4(172, 17, 0, 2), Port: 8080}, "one client for all"},
		{&net.TCPAddr{IP: net.IPv4zero, Port: 8080}, "one client for all"},
	} {
		if got := TrustedProxies(c.addr); !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q in it", c.addr, got, c.want)
		}
	}
	proxies, _ := ParseProxies("10.88.0.0/16, 192.0.2.7")
	got := TrustedProxies(&net.TCPAddr{IP: net.IPv4zero, Port: 8080}, proxies...)
	if !strings.Contains(got, "10.88.0.0/16, 192.0.2.7/32") || strings.Contains(got, "one client for all") {
		t.Errorf("with proxies: %q", got)
	}
}

// BehindProxies believes the headers from peers in its prefixes, walks
// X-Forwarded-For past every address in them, and leaves other peers and
// the loopback rules as they were.
func TestBehindProxies(t *testing.T) {
	proxies, err := ParseProxies("10.88.0.0/16, 192.0.2.7, 2001:db8:ffff::/48")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, remote, realIP string
		xff                  []string
		want, key            string
	}{
		{"proxy on the bridge", "10.88.0.1:4711", "", []string{"198.51.100.7"}, "198.51.100.7", "198.51.100.7"},
		{"a bare address is a /32", "192.0.2.7:4711", "", []string{"198.51.100.7"}, "198.51.100.7", "198.51.100.7"},
		{"X-Real-IP alone from the proxy", "10.88.0.1:4711", "198.51.100.7", nil, "198.51.100.7", "198.51.100.7"},
		{"forged X-Real-IP beside the proxy's X-Forwarded-For", "10.88.0.1:4711", "198.51.100.7", []string{"203.0.113.9"}, "203.0.113.9", "203.0.113.9"},
		{"chain: the walk skips the trusted proxies", "10.88.0.1:4711", "", []string{"203.0.113.9, 192.0.2.7, 10.88.3.4"}, "203.0.113.9", "203.0.113.9"},
		{"chain over several header lines", "10.88.0.1:4711", "", []string{"203.0.113.9", "192.0.2.7"}, "203.0.113.9", "203.0.113.9"},
		{"a forged address left of the client is not reached", "10.88.0.1:4711", "", []string{"198.51.100.8, 203.0.113.9, 10.88.3.4"}, "203.0.113.9", "203.0.113.9"},
		{"a client that names a trusted proxy names itself", "10.88.0.1:4711", "", []string{"10.88.0.9, 203.0.113.9"}, "203.0.113.9", "203.0.113.9"},
		{"every address trusted: the leftmost", "10.88.0.1:4711", "", []string{"10.88.0.9, 192.0.2.7"}, "10.88.0.9", "10.88.0.9"},
		{"garbage ends the walk at the last address reached", "10.88.0.1:4711", "", []string{"unknown, 10.88.3.4"}, "10.88.3.4", "10.88.3.4"},
		{"garbage last: the peer", "10.88.0.1:4711", "198.51.100.7", []string{"203.0.113.9, unknown"}, "10.88.0.1", "10.88.0.1"},
		{"IPv6 client keyed by its /64", "[2001:db8:ffff::2]:4711", "", []string{"2001:db8:1:2:3::9"}, "2001:db8:1:2:3::9", "2001:db8:1:2::/64"},
		{"IPv4-mapped proxy", "[::ffff:10.88.0.1]:4711", "", []string{"198.51.100.7"}, "198.51.100.7", "198.51.100.7"},
		{"untrusted peer: headers ignored", "10.0.0.2:4711", "198.51.100.7", []string{"192.0.2.1"}, "10.0.0.2", "10.0.0.2"},
		{"untrusted peer naming a trusted proxy", "203.0.113.9:4711", "", []string{"198.51.100.7, 10.88.0.1"}, "203.0.113.9", "203.0.113.9"},
		{"loopback as before", "127.0.0.1:4711", "", []string{"198.51.100.7"}, "198.51.100.7", "198.51.100.7"},
		{"loopback behind a trusted proxy", "127.0.0.1:4711", "", []string{"198.51.100.7, 10.88.0.1"}, "198.51.100.7", "198.51.100.7"},
		{"Unix socket as before", "@", "198.51.100.7", nil, "198.51.100.7", "198.51.100.7"},
		{"Unix socket without headers", "@", "", nil, "invalid IP", "local"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = c.remote
			if c.realIP != "" {
				r.Header.Set("X-Real-IP", c.realIP)
			}
			for _, v := range c.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			var ip, key string
			BehindProxies(proxies, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ip, key = ClientIP(r).String(), ClientKey(r)
			})).ServeHTTP(httptest.NewRecorder(), r)
			if ip != c.want || key != c.key {
				t.Errorf("ClientIP = %s, ClientKey = %s; want %s and %s", ip, key, c.want, c.key)
			}
		})
	}
	h := http.RedirectHandler("/", http.StatusFound)
	if BehindProxies(nil, h) != h {
		t.Error("BehindProxies without proxies wraps its handler")
	}
}

func TestParseProxies(t *testing.T) {
	for _, c := range []struct{ list, want string }{
		{"", "[]"},
		{"  ", "[]"},
		{"10.0.0.1", "[10.0.0.1/32]"},
		{"10.1.2.3/8, fd00::1", "[10.0.0.0/8 fd00::1/128]"},
		{"::ffff:10.0.0.1,::ffff:10.0.0.0/104", "[10.0.0.1/32 10.0.0.0/8]"},
	} {
		got, err := ParseProxies(c.list)
		if err != nil || fmt.Sprint(got) != c.want {
			t.Errorf("ParseProxies(%q) = %v, %v; want %s", c.list, got, err, c.want)
		}
	}
	for _, list := range []string{"10.0.0.1,", "proxy.local", "10.0.0.0/33", "10.0.0.1, ,10.0.0.2", "::ffff:0:0/64", "10.0.0.1:8080"} {
		if _, err := ParseProxies(list); err == nil {
			t.Errorf("ParseProxies(%q) took it", list)
		}
	}
}
