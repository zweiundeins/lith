package main

import "testing"

// TestMetricsListen: the metrics listen beside the site, port + 100 on
// loopback, and never on an address the public reaches.
func TestMetricsListen(t *testing.T) {
	for _, c := range []struct{ flag, addr, want string }{
		{"auto", "127.0.0.1:8097", "127.0.0.1:8197"},
		{"auto", "0.0.0.0:8097", "127.0.0.1:8197"},
		{"auto", "unix:/run/lith-site/lith-site.sock", ""},
		{"off", "127.0.0.1:8097", ""},
		{"127.0.0.1:9999", "127.0.0.1:8097", "127.0.0.1:9999"},
		{"[::1]:9999", "127.0.0.1:8097", "[::1]:9999"},
	} {
		got, err := metricsListen(c.flag, c.addr)
		if err != nil || got != c.want {
			t.Errorf("-metrics %s with -addr %s: %q, %v; want %q", c.flag, c.addr, got, err, c.want)
		}
	}
	for _, public := range []string{"0.0.0.0:8197", ":8197", "203.0.113.7:8197"} {
		if _, err := metricsListen(public, "127.0.0.1:8097"); err == nil {
			t.Errorf("-metrics %s: accepted", public)
		}
	}
}
