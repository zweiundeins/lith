package edge

import (
	"net/http/httptest"
	"testing"
)

func TestClientIPTrustsOnlyTheLocalProxy(t *testing.T) {
	for _, c := range []struct {
		name, remote, realIP, xff string
		want, key                 string
	}{
		{"direct client", "203.0.113.9:4711", "", "", "203.0.113.9", "203.0.113.9"},
		{"direct client forging X-Real-IP", "203.0.113.9:4711", "198.51.100.7", "", "203.0.113.9", "203.0.113.9"},
		{"direct client forging X-Forwarded-For", "203.0.113.9:4711", "", "198.51.100.7", "203.0.113.9", "203.0.113.9"},
		{"LAN peer is not the local proxy", "10.0.0.2:4711", "198.51.100.7", "", "10.0.0.2", "10.0.0.2"},
		{"proxy over loopback", "127.0.0.1:4711", "198.51.100.7", "", "198.51.100.7", "198.51.100.7"},
		{"proxy over IPv6 loopback", "[::1]:4711", "198.51.100.7", "", "198.51.100.7", "198.51.100.7"},
		{"proxy over a Unix socket", "@", "198.51.100.7", "", "198.51.100.7", "198.51.100.7"},
		{"proxy over an unnamed Unix socket", "", "198.51.100.7", "", "198.51.100.7", "198.51.100.7"},
		{"X-Real-IP wins over X-Forwarded-For", "127.0.0.1:4711", "198.51.100.7", "192.0.2.1", "198.51.100.7", "198.51.100.7"},
		{"last X-Forwarded-For hop is the proxy's peer", "127.0.0.1:4711", "", "192.0.2.1, 198.51.100.7", "198.51.100.7", "198.51.100.7"},
		{"garbage X-Real-IP falls back", "127.0.0.1:4711", "not an ip", "198.51.100.7", "198.51.100.7", "198.51.100.7"},
		{"loopback without headers", "127.0.0.1:4711", "", "", "127.0.0.1", "127.0.0.1"},
		{"Unix socket without headers", "@", "", "", "invalid IP", "local"},
		{"IPv4-mapped IPv6", "[::ffff:203.0.113.9]:4711", "", "", "203.0.113.9", "203.0.113.9"},
		{"IPv6 keyed by its /64", "[2001:db8:1:2:3:4:5:6]:4711", "", "", "2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		{"IPv6 from the proxy keyed by its /64", "127.0.0.1:4711", "2001:db8:1:2:ffff::1", "", "2001:db8:1:2:ffff::1", "2001:db8:1:2::/64"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = c.remote
			if c.realIP != "" {
				r.Header.Set("X-Real-IP", c.realIP)
			}
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
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
