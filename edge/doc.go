// Package edge is what the apps do at the request edge, before a handler runs
// and after it has written. It never touches the database: a session here is
// a cookie, and what a session holds is the app's to store through its writer.
//
// Where each piece comes from:
//
//	ClientIP, ClientKey  starbase and swisscyberguard: proxy headers count only from loopback or a Unix socket
//	Sessions             starbase's cookie-only session, hyperlith's __Host-sid, swisscyberguard's 256-bit tokens
//	CSRF                 Go's CrossOriginProtection rules, strict on missing headers like hyperlith and starbase, kanban's Datastar-Request check
//	SecurityHeaders, CSP starbase's per-request nonce, hyperlith's policy as data, kanban's and swisscyberguard's other headers
//	Compress             starbase's GET and HEAD rule, kanban's Accept-Encoding parsing, and an Unwrap where libretto and swisscyberguard kept bypass lists
//	Static, Precompress  starbase's precompress package, libretto's content ETags with no-cache, kanban's hashed immutable URLs
//	Limiter              starbase's token bucket, swept as it goes, and libretto's rule that a clock step must not reset it
package edge
