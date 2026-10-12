// Package edge is what the apps do at the request edge, before a handler runs
// and after it has written. It never touches the database: a session here is
// a cookie, and what a session holds is the app's to store through its writer.
//
// Where each piece comes from:
//
//	ClientIP, ClientKey  starbase and swisscyberguard: proxy headers count only from loopback or a Unix socket
//	Sessions             starbase's cookie-only session, hyperlith's __Host-sid, swisscyberguard's 256-bit tokens
//	CSRF                 Go's CrossOriginProtection rules, strict on missing headers like hyperlith and starbase, kanban's Datastar-Request check
package edge
