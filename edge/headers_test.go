package edge

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCSPHeader(t *testing.T) {
	got := DefaultCSP().Header("n0nce")
	want := "base-uri 'self'; connect-src 'self'; default-src 'self'; font-src 'self'; form-action 'self'; " +
		"frame-ancestors 'none'; img-src 'self' data:; object-src 'none'; " +
		"script-src 'self' 'nonce-n0nce' 'unsafe-eval'; style-src 'self' 'unsafe-inline'"
	if got != want {
		t.Errorf("DefaultCSP().Header:\n got %s\nwant %s", got, want)
	}
	c := CSP{"default-src": {"'self'"}, "upgrade-insecure-requests": nil, "style-src": {NonceSource, NonceSource}}
	if got := c.Header("x"); got != "default-src 'self'; style-src 'nonce-x' 'nonce-x'; upgrade-insecure-requests" {
		t.Errorf("Header: %s", got)
	}
}

func TestSecurityHeadersNonce(t *testing.T) {
	var nonces []string
	h := SecurityHeaders{CSP: DefaultCSP()}.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonces = append(nonces, Nonce(r.Context()))
	}))
	var headers []string
	for range 2 {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		headers = append(headers, w.Header().Get("Content-Security-Policy"))
	}
	for i, n := range nonces {
		if len(n) != 22 || !strings.Contains(headers[i], "'nonce-"+n+"'") {
			t.Errorf("request %d: nonce %q not in its policy %q", i, n, headers[i])
		}
	}
	if nonces[0] == nonces[1] {
		t.Error("two requests got the same nonce")
	}

	// A policy without a nonce source makes none.
	h = SecurityHeaders{CSP: CSP{"default-src": {"'self'"}}}.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := Nonce(r.Context()); n != "" {
			t.Errorf("nonce %q without a nonce source", n)
		}
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if got := w.Header().Get("Content-Security-Policy"); got != "default-src 'self'" {
		t.Errorf("policy %q", got)
	}
}

func TestSecurityHeaders(t *testing.T) {
	serve := func(s SecurityHeaders, handler http.HandlerFunc) http.Header {
		if handler == nil {
			handler = func(http.ResponseWriter, *http.Request) {}
		}
		w := httptest.NewRecorder()
		s.Handler(handler).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		return w.Header()
	}

	h := serve(SecurityHeaders{CSP: DefaultCSP()}, nil)
	for k, v := range map[string]string{
		"X-Content-Type-Options":     "nosniff",
		"Referrer-Policy":            "strict-origin-when-cross-origin",
		"Cross-Origin-Opener-Policy": "same-origin",
		"X-Frame-Options":            "DENY",
		"Strict-Transport-Security":  "",
	} {
		if got := h.Get(k); got != v {
			t.Errorf("%s: %q, want %q", k, got, v)
		}
	}

	csp := DefaultCSP()
	csp["frame-ancestors"] = []string{"'self'"}
	h = serve(SecurityHeaders{CSP: csp, HSTS: true, Referrer: "same-origin", Extra: map[string]string{
		"Permissions-Policy":         "camera=(), microphone=()",
		"Cross-Origin-Opener-Policy": "",
	}}, nil)
	for k, v := range map[string]string{
		"X-Frame-Options":            "SAMEORIGIN",
		"Strict-Transport-Security":  "max-age=63072000; includeSubDomains",
		"Referrer-Policy":            "same-origin",
		"Permissions-Policy":         "camera=(), microphone=()",
		"Cross-Origin-Opener-Policy": "",
	} {
		if got := h.Get(k); got != v {
			t.Errorf("%s: %q, want %q", k, got, v)
		}
	}

	csp["frame-ancestors"] = []string{"https://partner.test"}
	if h := serve(SecurityHeaders{CSP: csp}, nil); h.Get("X-Frame-Options") != "" {
		t.Errorf("X-Frame-Options %q for a framing partner", h.Get("X-Frame-Options"))
	}
	if h := serve(SecurityHeaders{}, nil); h.Get("Content-Security-Policy") != "" || h.Get("X-Frame-Options") != "" {
		t.Error("no CSP configured, yet a policy was sent")
	}

	// A handler has the last word for its own response.
	h = serve(SecurityHeaders{CSP: DefaultCSP()}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	})
	if h.Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Error("the handler could not override a header")
	}
}
