package edge

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// CSRF refuses cross-site requests with unsafe methods (all but GET, HEAD and
// OPTIONS) with 403. Such a request passes when the browser's Sec-Fetch-Site
// says same-origin, or none (the user started it). Without that header, as
// from browsers older than 2023, its Origin must name this host. A request
// with neither header is refused: other clients must send an Origin. A
// same-site or cross-site request passes only from one of Origins, since a
// sibling subdomain is not this site.
type CSRF struct {
	// Origins are other origins trusted to send such requests, each as
	// "scheme://host[:port]".
	Origins []string
	// Exempt are ServeMux patterns, such as "POST /hooks/{name}", whose
	// requests skip the check.
	Exempt []string
	// Datastar are ServeMux patterns of action routes, such as "POST /cmd/",
	// whose requests must carry Datastar-Request: true as well. A form cannot
	// send that header, and a cross-site fetch only after a preflight.
	Datastar []string
}

// Handler returns next behind the check. Like ServeMux, it panics on a
// pattern that is not valid, and on an origin that is not one.
func (c CSRF) Handler(next http.Handler) http.Handler {
	trusted := map[string]bool{}
	for _, o := range c.Origins {
		trusted[trustedOrigin(o)] = true
	}
	exempt, datastar := patternSet(c.Exempt), patternSet(c.Datastar)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if exempt.match(r) {
				break
			}
			if !sameOrigin(r, trusted) {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
			if datastar.match(r) && r.Header.Get("Datastar-Request") != "true" {
				http.Error(w, "Datastar-Request header required", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameOrigin(r *http.Request, trusted map[string]bool) bool {
	origin := strings.ToLower(r.Header.Get("Origin"))
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "":
		if u, err := url.Parse(origin); err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host) {
			return true
		}
	}
	return trusted[origin]
}

func trustedOrigin(o string) string {
	u, err := url.Parse(strings.TrimSuffix(o, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		panic(fmt.Sprintf("edge: CSRF origin %q is not scheme://host[:port]", o))
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// patterns matches requests against ServeMux patterns. Only a direct match
// counts: a path that ServeMux would clean or redirect to a pattern does not.
type patterns struct{ mux *http.ServeMux }

type mark struct{}

func (*mark) ServeHTTP(http.ResponseWriter, *http.Request) {}

var matched = &mark{}

func patternSet(ps []string) patterns {
	if len(ps) == 0 {
		return patterns{}
	}
	mux := http.NewServeMux()
	for _, p := range ps {
		mux.Handle(p, matched)
	}
	return patterns{mux}
}

func (p patterns) match(r *http.Request) bool {
	if p.mux == nil {
		return false
	}
	h, _ := p.mux.Handler(r)
	return h == matched
}
