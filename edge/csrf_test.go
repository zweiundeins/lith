package edge

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCSRF(t *testing.T) {
	h := CSRF{
		Origins:  []string{"https://admin.example.com/"},
		Exempt:   []string{"POST /hooks/{name}"},
		Datastar: []string{"POST /cmd/"},
	}.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	const (
		pass = http.StatusOK
		deny = http.StatusForbidden
	)
	for _, c := range []struct {
		name, method, path string
		site, origin       string
		datastar           bool
		want               int
	}{
		{"GET is safe from anywhere", "GET", "/form", "cross-site", "https://evil.test", false, pass},
		{"HEAD is safe", "HEAD", "/form", "cross-site", "", false, pass},
		{"OPTIONS is safe", "OPTIONS", "/form", "cross-site", "https://evil.test", false, pass},

		{"same-origin", "POST", "/form", "same-origin", "https://example.com", false, pass},
		{"user-initiated (none)", "POST", "/form", "none", "", false, pass},
		{"same-site subdomain", "POST", "/form", "same-site", "https://blog.example.com", false, deny},
		{"cross-site", "POST", "/form", "cross-site", "https://evil.test", false, deny},
		{"cross-site from a trusted origin", "POST", "/form", "cross-site", "https://admin.example.com", false, pass},
		{"cross-site naming this host (http to https)", "POST", "/form", "cross-site", "http://example.com", false, deny},

		{"old browser, Origin is this host", "POST", "/form", "", "https://example.com", false, pass},
		{"old browser, Origin in other case", "POST", "/form", "", "https://EXAMPLE.com", false, pass},
		{"old browser, foreign Origin", "POST", "/form", "", "https://evil.test", false, deny},
		{"old browser, trusted Origin", "POST", "/form", "", "https://admin.example.com", false, pass},
		{"old browser, opaque Origin", "POST", "/form", "", "null", false, deny},
		{"missing both headers", "POST", "/form", "", "", false, deny},
		{"PUT without headers", "PUT", "/form", "", "", false, deny},
		{"PATCH without headers", "PATCH", "/form", "", "", false, deny},
		{"DELETE without headers", "DELETE", "/form", "", "", false, deny},

		{"exempt path, cross-site", "POST", "/hooks/stripe", "cross-site", "https://evil.test", false, pass},
		{"exempt path, missing headers", "POST", "/hooks/stripe", "", "", false, pass},
		{"below the exempt pattern", "POST", "/hooks/stripe/x", "", "", false, deny},
		{"exempt only after path cleaning", "POST", "/cmd/../hooks/stripe", "", "", false, deny},
		{"exempt pattern is POST only", "PUT", "/hooks/stripe", "", "", false, deny},

		{"action without Datastar-Request", "POST", "/cmd/move", "same-origin", "", false, deny},
		{"action with Datastar-Request", "POST", "/cmd/move", "same-origin", "", true, pass},
		{"cross-site action with Datastar-Request", "POST", "/cmd/move", "cross-site", "https://evil.test", true, deny},
		{"non-action needs no Datastar-Request", "POST", "/login", "same-origin", "", false, pass},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, "http://example.com"+c.path, nil)
			r.URL.Path = c.path // keep "/../" as sent
			if c.site != "" {
				r.Header.Set("Sec-Fetch-Site", c.site)
			}
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if c.datastar {
				r.Header.Set("Datastar-Request", "true")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Errorf("%s %s: %d, want %d (%s)", c.method, c.path, w.Code, c.want, w.Body)
			}
		})
	}
}

func TestCSRFOriginWithPort(t *testing.T) {
	h := CSRF{}.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for origin, want := range map[string]int{"http://localhost:8080": 200, "http://localhost:3000": 403, "http://localhost": 403} {
		r := httptest.NewRequest("POST", "http://localhost:8080/x", nil)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("Origin %s to localhost:8080: %d, want %d", origin, w.Code, want)
		}
	}
}

func TestCSRFRefusesBadConfig(t *testing.T) {
	for _, c := range []CSRF{
		{Origins: []string{"https://example.com/app"}},
		{Origins: []string{"example.com"}},
		{Origins: []string{"null"}},
		{Origins: []string{"https://user@example.com"}},
		{Exempt: []string{"POST /hooks/{name"}},
		{Datastar: []string{"/cmd/", "/cmd/"}},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%+v: no panic", c)
				}
			}()
			c.Handler(http.NotFoundHandler())
		}()
	}
}
