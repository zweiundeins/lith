package edge

import (
	"net/http/httptest"
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
