// Package edge is what the apps do at the request edge, before a handler runs
// and after it has written. It never touches the database: a session here is
// a cookie, and what a session holds is the app's to store through its writer.
//
// Where each piece comes from:
//
//	ClientIP, ClientKey  starbase: proxy headers count only from loopback or a Unix socket, X-Forwarded-For before X-Real-IP
//	Sessions             starbase's cookie-only session, hyperlith's __Host-sid, 256-bit tokens
//	CSRF                 Go's CrossOriginProtection rules, strict on missing headers like hyperlith and starbase, kanban's Datastar-Request check
//	SecurityHeaders, CSP starbase's per-request nonce and sources, hyperlith's policy as data, kanban's other headers
//	Compress, Negotiate  starbase's GET and HEAD rule, kanban's Accept-Encoding parsing, and an Unwrap instead of a list of paths that bypass it
//	Static, Precompress  starbase's precompress package, content ETags with no-cache, kanban's hashed immutable URLs
//	Minify               lith's site, which minified its scripts and its CSS bundle with tdewolff/minify before naming them
//	Limiter              starbase's token bucket, swept as it goes, a clock step that resets and refills nothing, and a sliding window for five messages an hour
//	Health, Ready        liveness and readiness probes, with checks registered by the app
//	Recover, LogRequests starbase's and kanban's recoverer and request log, on slog
//	SlowLink             lith's site: the page's own measurement, Save-Data and Chromium's network hints
//
// A chain, built from the inside out:
//
//	h := http.Handler(mux)
//	h = sessions.Handler(h)      // inside CSRF: a refused request gets no cookie
//	h = csrf.Handler(h)
//	h = security.Handler(h)
//	h = edge.Compress(h)
//	h = edge.Recover(log, h)     // around Compress: a panic drops the held body
//	h = edge.LogRequests(log, h) // around Recover: it logs the 500
package edge
