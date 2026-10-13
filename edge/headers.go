package edge

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"maps"
	"net/http"
	"slices"
	"strings"
)

// CSP is a Content-Security-Policy: each directive with its sources.
type CSP map[string][]string

// NonceSource stands for the request's 'nonce-...' source among a directive's
// sources.
const NonceSource = "'nonce'"

// DefaultCSP is a policy for a Datastar app. Everything comes from its own
// origin; scripts also by nonce, for an import map or an inline script.
// 'unsafe-eval' is required: Datastar compiles its expressions with Function.
// Inline styles are allowed for components that render <style> into their
// shadow roots; a nonce in style-src would turn 'unsafe-inline' off. Nothing
// may frame the site.
func DefaultCSP() CSP {
	return CSP{
		"default-src":     {"'self'"},
		"script-src":      {"'self'", NonceSource, "'unsafe-eval'"},
		"style-src":       {"'self'", "'unsafe-inline'"},
		"img-src":         {"'self'", "data:"},
		"font-src":        {"'self'"},
		"connect-src":     {"'self'"},
		"object-src":      {"'none'"},
		"base-uri":        {"'self'"},
		"form-action":     {"'self'"},
		"frame-ancestors": {"'none'"},
	}
}

// Header returns the policy as a header value, with nonce in place of
// NonceSource.
func (c CSP) Header(nonce string) string {
	return strings.Join(c.parts(), nonce)
}

// parts is the header value split where nonces go.
func (c CSP) parts() []string {
	var parts []string
	var b strings.Builder
	for i, name := range slices.Sorted(maps.Keys(c)) {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(name)
		for _, src := range c[name] {
			b.WriteByte(' ')
			if src != NonceSource {
				b.WriteString(src)
				continue
			}
			b.WriteString("'nonce-")
			parts = append(parts, b.String())
			b.Reset()
			b.WriteByte('\'')
		}
	}
	return append(parts, b.String())
}

// SecurityHeaders sets the security headers on every response, and puts a
// fresh nonce for its CSP in each request's context (see Nonce). A handler can
// still change any of them for its own response.
type SecurityHeaders struct {
	// CSP is the Content-Security-Policy, none when nil. Its frame-ancestors
	// also sets X-Frame-Options, for browsers that predate it.
	CSP CSP
	// HSTS sends Strict-Transport-Security for two years, subdomains
	// included. Set it when the site is served over HTTPS.
	HSTS bool
	// HSTSNoSubdomains leaves the subdomains out of HSTS, for a domain whose
	// subdomains are not all served over HTTPS, such as a mail host on plain
	// HTTP.
	HSTSNoSubdomains bool
	// Referrer is the Referrer-Policy, strict-origin-when-cross-origin when
	// empty.
	Referrer string
	// Extra headers to set on every response after the others, such as
	// Permissions-Policy. An empty value removes the header.
	Extra map[string]string
}

type nonceKey struct{}

// Handler returns next behind the headers.
func (s SecurityHeaders) Handler(next http.Handler) http.Handler {
	var csp []string
	if s.CSP != nil {
		csp = s.CSP.parts()
	}
	set := [][2]string{
		{"X-Content-Type-Options", "nosniff"},
		{"Referrer-Policy", cmp.Or(s.Referrer, "strict-origin-when-cross-origin")},
		{"Cross-Origin-Opener-Policy", "same-origin"},
	}
	switch strings.Join(s.CSP["frame-ancestors"], " ") {
	case "'none'":
		set = append(set, [2]string{"X-Frame-Options", "DENY"})
	case "'self'":
		set = append(set, [2]string{"X-Frame-Options", "SAMEORIGIN"})
	}
	if s.HSTS {
		hsts := "max-age=63072000; includeSubDomains"
		if s.HSTSNoSubdomains {
			hsts = "max-age=63072000"
		}
		set = append(set, [2]string{"Strict-Transport-Security", hsts})
	}
	for _, k := range slices.Sorted(maps.Keys(s.Extra)) {
		set = append(set, [2]string{k, s.Extra[k]})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		if csp != nil {
			nonce := ""
			if len(csp) > 1 {
				b := make([]byte, 16)
				rand.Read(b)
				nonce = base64.RawURLEncoding.EncodeToString(b)
				r = r.WithContext(context.WithValue(r.Context(), nonceKey{}, nonce))
			}
			h.Set("Content-Security-Policy", strings.Join(csp, nonce))
		}
		for _, kv := range set {
			if kv[1] == "" {
				h.Del(kv[0])
			} else {
				h.Set(kv[0], kv[1])
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Nonce returns the CSP nonce SecurityHeaders put in ctx, or "" when its
// policy has none. An inline script carries it as <script nonce="...">.
func Nonce(ctx context.Context) string {
	n, _ := ctx.Value(nonceKey{}).(string)
	return n
}
