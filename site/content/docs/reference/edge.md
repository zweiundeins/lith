---
title: edge
headline: lith/edge: sessions, CSRF, compression, static files
summary: What the apps do at the request edge, before a handler runs and after it has written.
description: Reference for lith/edge: sessions, CSRF checks, a CSP with a nonce, client addresses, rate limits, compression and precompressed static files.
order: 7
---

Import path `github.com/zweiundeins/lith/edge`. Generated from the doc comments in [edge/](https://github.com/zweiundeins/lith/tree/main/edge).

It never touches the database: a session here is a cookie, and what a session holds is the app's to store through its writer.

Where each piece comes from:

```text
ClientIP, ClientKey  starbase: proxy headers count only from loopback or a Unix socket, X-Forwarded-For before X-Real-IP
Sessions             starbase's cookie-only session, hyperlith's __Host-sid, 256-bit tokens
CSRF                 Go's CrossOriginProtection rules, strict on missing headers like hyperlith and starbase, kanban's Datastar-Request check
SecurityHeaders, CSP starbase's per-request nonce and sources, hyperlith's policy as data, kanban's other headers
Compress, Negotiate  starbase's GET and HEAD rule, kanban's Accept-Encoding parsing, and an Unwrap instead of a list of paths that bypass it
Static, Precompress  starbase's precompress package, content ETags with no-cache, kanban's hashed immutable URLs
Minify               lith's site, which minified its scripts and its CSS bundle with tdewolff/minify before naming them
Limiter              starbase's token bucket, swept as it goes, a clock step that resets and refills nothing, and a sliding window for five messages an hour
Health, Ready        liveness and readiness probes, with checks registered by the app
Recover, LogRequests starbase's and kanban's recoverer and request log, on slog
SlowLink             lith's site: the page's own measurement, Save-Data and Chromium's network hints
Upload, TooLarge     lith's v0.5.0 review and Libretto: an upload's deadline that moves while the body arrives at a minimum rate
```

A chain, built from the inside out:

```go
h := http.Handler(mux)
h = sessions.Handler(h)      // inside CSRF: a refused request gets no cookie
h = csrf.Handler(h)
h = security.Handler(h)
h = edge.Compress(h)
h = edge.Recover(log, h)     // around Compress: a panic drops the held body
h = edge.LogRequests(log, h) // around Recover: it logs the 500
```

The server's handler is the mux that serves h, the probes and the static files. Its ReadTimeout bounds every request's body, on every route, read or not, and leaves streams alone: over HTTP/1.1 Go's server clears the read deadline once the body is in, or before the handler runs for a request without one, and over HTTP/2 the timeout is a timer per stream that does nothing once the body has ended. An upload's handler extends it with Upload.

```go
srv := &http.Server{Handler: top, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 2 * time.Minute}
```

## Constants

```go
const (
	// UploadWindow is the time an upload's body gets at first, and again
	// each time it has sent MinRate for that long.
	UploadWindow = 10 * time.Second

	// UploadMinRate is a minimum rate for an upload over a slow link, in
	// bytes a second: three uploads sharing a 0.4 Mbit/s mobile link get
	// about 133 kbit/s each, some 16 KB/s, twice its 8 KiB/s, while a
	// client that trickles a byte a second is cut off after a window.
	UploadMinRate = 8 << 10

	// UploadMax is the default size limit of an upload, 10 MiB.
	UploadMax = 10 << 20

	// UploadSlack is added to Max/MinRate for an upload's longest time, so
	// that an upload of the largest size at the lowest rate, with the
	// stalls of a mobile link, still finishes.
	UploadSlack = time.Minute
)
```

Upload defaults.

```go
const NetworkHints = "ECT, RTT, Downlink"
```

NetworkHints are the client hints SlowLink reads besides Save-Data, for an Accept-CH header on documents: Chromium then sends them with the requests that follow, the page's stream among them. Other browsers send none, and the page's own measurement (Save-Data: on on its stream's request) stands in for them.

```go
const NonceSource = "'nonce'"
```

NonceSource stands for the request's 'nonce-...' source among a directive's sources.

## Functions

### func BrotliLen {#BrotliLen}

```go
func BrotliLen(b []byte) int
```

BrotliLen is the length of b under brotli -11, as Precompress compresses it, measured without keeping the bytes: for bytes that are only measured, never served.

### func ClientIP {#ClientIP}

```go
func ClientIP(r *http.Request) netip.Addr
```

ClientIP returns the address of the client that sent r. A peer on loopback or a Unix socket is the local reverse proxy, which names the client as the last address of X-Forwarded-For. The proxy must set that header: Caddy's reverse\_proxy does by default, nginx with proxy\_set\_header X-Forwarded-For $proxy\_add\_x\_forwarded\_for. X-Real-IP counts only when there is no X-Forwarded-For: a proxy may pass on an X-Real-IP the client sent, as Caddy does unless the site block sets it. From any other peer both headers are ignored: a client can send them itself, to dodge its rate limit or to spend someone else's. When the proxy names no address that parses, the address is the peer's, invalid for a Unix socket.

### func ClientKey {#ClientKey}

```go
func ClientKey(r *http.Request) string
```

ClientKey is ClientIP as a rate-limit key. An IPv6 client counts by its /64, since one subscriber holds a whole /64 and could rotate through it; an unknown address is "local".

### func Compress {#Compress}

```go
func Compress(next http.Handler) http.Handler
```

Compress compresses the responses to GET, HEAD and QUERY with zstd, brotli or gzip, whichever the request accepts best (in that order when it accepts them equally: zstd takes about an eighth of brotli's time for a page, for about a sixth more bytes; Safari before 26.3 sends no zstd and gets brotli). It leaves alone a response that sets its own Content-Encoding (precompressed files, streams that compress themselves), event streams, partial content, types that don't compress, bodies under 512 bytes, and responses with Cache-Control: no-transform. Other methods pass untouched: Datastar's POST streams compress themselves, across frames.

A Flush sends everything written so far to the client. The writer unwraps for http.ResponseController, so a stream can still set its deadlines.

### func Health {#Health}

```go
func Health(version string) http.Handler
```

Health answers 200 "ok \<version>" while the process serves. It checks nothing, so a busy database never fails it. Mount it at /healthz.

### func LogRequests {#LogRequests}

```go
func LogRequests(log *slog.Logger, next http.Handler) http.Handler
```

LogRequests logs a line for each request as it ends: method, path, status, bytes, duration and client. A 5xx, or a response a panic cut off, logs at Error, the rest at Info, or at Debug when the handler called Quiet. Mount it outside Recover, so it logs the 500.

### func Negotiate {#Negotiate}

```go
func Negotiate(acceptEncoding string, codings ...string) string
```

Negotiate returns the one of codings that an Accept-Encoding header rates highest, earlier ones winning ties, or "" when it accepts none of them. Without codings it chooses among zstd, br and gzip, as Compress does. A handler with prerendered variants names the ones it has, such as br and gzip from Precompress.

### func Nonce {#Nonce}

```go
func Nonce(ctx context.Context) string
```

Nonce returns the CSP nonce SecurityHeaders put in ctx, or "" when its policy has none. An inline script carries it as \<script nonce="...">.

### func Precompress {#Precompress}

```go
func Precompress(b []byte) (br, gz []byte)
```

Precompress returns b under brotli -11 and gzip -9, compressed once per content: callers with the same bytes wait for the one compression and later ones get it at once. The gzip is the standard library's compress/gzip, byte for byte, so a size an app measures with that package is the size served. The cache keeps every content for the life of the process, so it is for bytes served many times: static files and generated assets.

### func Quiet {#Quiet}

```go
func Quiet(r *http.Request)
```

Quiet tells LogRequests that the request is one of many alike, such as the moves of a pointer that a page sends several times a second: its line goes out at Debug instead of Info, unless it fails with a 5xx or is cut off, which still log at Error. Outside LogRequests it does nothing.

### func Recover {#Recover}

```go
func Recover(log *slog.Logger, next http.Handler) http.Handler
```

Recover turns a panic in next into a 500, logged with its stack. When the response has already begun, it aborts the connection instead, so the client can tell the response is incomplete.

### func ServePrecompressed {#ServePrecompressed}

```go
func ServePrecompressed(w http.ResponseWriter, r *http.Request, body []byte)
```

ServePrecompressed answers with body in the best of Precompress's codings the request accepts. Bodies under 512 bytes, types that don't compress and requests that accept neither coding get it as it is. It sets an ETag of the content and leaves conditional and range requests to http.ServeContent, so an If-None-Match that names the ETag gets a 304. Set Content-Type and Cache-Control before; a Content-Encoding it sets keeps Compress out of the way.

Like Precompress, it hashes and compresses each content once and keeps the result, the body included, for the life of the process: it is for bytes served many times, such as prerendered pages.

### func SessionID {#SessionID}

```go
func SessionID(ctx context.Context) string
```

SessionID returns the session id Sessions.Handler put in ctx, or "".

### func SlowLink {#SlowLink}

```go
func SlowLink(r *http.Request) bool
```

SlowLink reports whether r says its client is on a slow link. Any of these does: the header Save-Data: on, which a browser sends when its user asked to save data, and a lith page sends on its stream's request from its own measurement (navigator.connection's saveData or an effective type of 2g or 3g, or where that is missing, a document that took over 300 ms to its first byte or came at under 100 KB/s), or once it fell behind its stream (live.NewestFrame); Chromium's client hints, after an Accept-CH of NetworkHints: ECT slow-2g, 2g or 3g, RTT over 300 (ms), Downlink under 0.8 (Mbit/s); the query parameter net=slow, which pages put in their stream's URL before they sent Save-Data, for those still open from then. A client that claims a slow link without one only gets fewer frames (live.Spec.Slow), which costs the server less.

### func TooLarge {#TooLarge}

```go
func TooLarge(err error) bool
```

TooLarge reports whether err comes from a body over its limit: an \*http.MaxBytesError, as Upload's body and http.MaxBytesReader return, also through ParseMultipartForm, MultipartReader's parts and a json.Decoder.

### func TrustedProxies {#TrustedProxies}

```go
func TrustedProxies(addr net.Addr) string
```

TrustedProxies says, for a log line at start, whose X-Forwarded-For and X-Real-IP ClientIP believes on a server that listens at addr (a listener's Addr). ClientIP believes peers on loopback and on a Unix socket: every local user who can connect to them can name any client address, and so dodge a per-address limit or spend someone else's. A socket whose mode lets only the proxy's group in (0660, the app's group holding the proxy's user) narrows that to the proxy. A proxy on another host, or on a container bridge, is not believed: every client then counts as the proxy's address, and one abuser spends the limit of all.

### func Upload {#Upload}

```go
func Upload(w http.ResponseWriter, r *http.Request, l UploadLimits) (io.ReadCloser, error)
```

Upload wraps r.Body for a handler that takes a large body, such as an upload, whose body the server's ReadTimeout would cut off:

```text
body, err := edge.Upload(w, r, edge.UploadLimits{Max: 50 << 20})
if err != nil {
	log.Warn("upload without a deadline", "err", err)
}
r.Body = body
if err := r.ParseMultipartForm(1 << 20); edge.TooLarge(err) { ... }
```

The body gets a Window, and each time it has sent MinRate\*Window bytes it gets a Window from then, until Max/MinRate+Slack from the call: a slow but honest upload takes as long as it needs, while a body that falls below MinRate is cut off a Window after the last time it kept up, a burst before a stall included. A body cut off fails its Read with an error that matches os.ErrDeadlineExceeded, and over HTTP/1.1 the server closes the connection after the answer. Reads past Max fail with an \*http.MaxBytesError (see TooLarge).

The server's WriteTimeout, if it has one, would fail the answer to an upload that takes longer, so Upload clears the write deadline while the body arrives, and sets it to the server's WriteTimeout from the body's end (or its failure), for the answer.

It works with ParseMultipartForm, MultipartReader and plain reads, over HTTP/1.1 and HTTP/2. Call it before the first Read. It sets the deadlines through http.ResponseController, so w must unwrap to the server's writer (lith's Compress and LogRequests do; an app's own wrapper needs an Unwrap method returning the writer it wraps). When one cannot be set, such as with httptest's recorder, err says so, wrapping http.ErrNotSupported, and the body, still capped at Max, is usable without it. It sets no read deadline after the body's end or a failed read: over HTTP/1.1 Go's server then watches the connection for a client that leaves, and a read deadline set on it would cancel the request's context when it passed. Clearing it (the zero time, as ReadSignals does) is harmless. A write deadline has no such effect.

### func UploadBody {#UploadBody}

```go
func UploadBody(w http.ResponseWriter, r *http.Request, minRate, maxSize int64) (io.ReadCloser, error)
```

UploadBody is Upload with a minimum rate and a size limit, and the default window and slack.

## Types

### type CSP {#CSP}

```go
type CSP map[string][]string
```

CSP is a Content-Security-Policy: each directive with its sources.

#### func DefaultCSP {#DefaultCSP}

```go
func DefaultCSP() CSP
```

DefaultCSP is a policy for a Datastar app. Everything comes from its own origin; scripts also by nonce, for an import map or an inline script. 'unsafe-eval' is required: Datastar compiles its expressions with Function. Inline styles are allowed for components that render \<style> into their shadow roots; a nonce in style-src would turn 'unsafe-inline' off. Nothing may frame the site.

#### func (CSP) Header {#CSP.Header}

```go
func (c CSP) Header(nonce string) string
```

Header returns the policy as a header value, with nonce in place of NonceSource.

### type CSRF {#CSRF}

```go
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
```

CSRF refuses cross-site requests with unsafe methods (all but GET, HEAD and OPTIONS) with 403. Such a request passes when the browser's Sec-Fetch-Site says same-origin, or none (the user started it). Without that header, as from browsers older than 2023, its Origin must name this host. A request with neither header is refused: other clients must send an Origin. A same-site or cross-site request passes only from one of Origins, since a sibling subdomain is not this site.

#### func (CSRF) Handler {#CSRF.Handler}

```go
func (c CSRF) Handler(next http.Handler) http.Handler
```

Handler returns next behind the check. Like ServeMux, it panics on a pattern that is not valid, and on an origin that is not one.

### type Limiter {#Limiter}

```go
type Limiter struct {
	// contains filtered or unexported fields
}
```

Limiter limits what each key may take, as a token bucket (NewLimiter), a fixed window (NewWindow) or a sliding one (NewSliding). A bucket that has filled up again, or a window that has closed, is the same as a new one, so Allow sweeps those out as it goes: memory holds the keys that were limited within about the last burst/rate seconds, or the last window.

Keys a client chooses, such as session ids, cost it nothing to change: check a per-IP limiter (ClientKey) first, so only requests it lets through make buckets in the other.

#### func NewLimiter {#NewLimiter}

```go
func NewLimiter(rate float64, burst int) *Limiter
```

NewLimiter returns a limiter that allows burst tokens at once and rate tokens a second after that. It panics unless rate > 0 and burst >= 1.

#### func NewSliding {#NewSliding}

```go
func NewSliding(n int, per time.Duration) *Limiter
```

NewSliding returns a limiter that allows n tokens in any span of length per: a token counts until per has passed since it was taken. Where NewWindow lets up to 2n through around the edge between two windows, a sliding window keeps a promise such as "five messages in the last hour" at every moment. A key keeps the uses of the last span, at most n of them; for n over 64 it counts the uses within each 64th of per together, from the end of that 64th, which bounds a key to 65 uses and gives tokens back up to per/64 late. It panics unless n >= 1 and per > 0.

#### func NewWindow {#NewWindow}

```go
func NewWindow(n int, per time.Duration) *Limiter
```

NewWindow returns a limiter that allows n tokens per window of length per. A key's window opens with its first use, and the first use once per has passed opens the next one, with n tokens again. Where a bucket gives tokens back as time passes, a window keeps a promise such as "five messages an hour": never more than n in one window. Around the edge between two windows, though, up to 2n go through within less than per; NewSliding keeps n in every span of per. It panics unless n >= 1 and per > 0.

#### func (*Limiter) Allow {#Limiter.Allow}

```go
func (l *Limiter) Allow(key string, n int, now time.Time) bool
```

Allow takes n tokens from key's bucket or window at now, and reports whether it had them. Pass time.Now(): its monotonic reading keeps a step of the wall clock from refilling or resetting a bucket. A time without one that steps back pauses the refill until the clock is past the bucket's last use again, keeps a window open until the clock passes its end, and counts a sliding window's uses until the clock is per past them, so the limiter turns stricter, never looser.

#### func (*Limiter) Len {#Limiter.Len}

```go
func (l *Limiter) Len() int
```

Len is the number of buckets held.

### type Ready {#Ready}

```go
type Ready struct {
	// Timeout bounds all checks together, 2 seconds when zero.
	Timeout time.Duration
	// Log gets each failure with its error, slog.Default() when nil.
	Log *slog.Logger

}
```

Ready answers 200 when every check passes within Timeout, and 503 naming those that did not, so the proxy takes the instance out of rotation. The checks run in parallel, with a context that ends at the timeout, and the probe answers by then even if a check ignores it.

A check must not queue behind the writer: a command waits for every batch ahead of it, which turns a busy writer into a failed probe. Read from the read pool (SELECT 1), or look at state in memory.

#### func (*Ready) Add {#Ready.Add}

```go
func (rd *Ready) Add(name string, check func(context.Context) error)
```

Add registers a check under a name for the 503 and the log.

#### func (*Ready) ServeHTTP {#Ready.ServeHTTP}

```go
func (rd *Ready) ServeHTTP(w http.ResponseWriter, r *http.Request)
```

### type SecurityHeaders {#SecurityHeaders}

```go
type SecurityHeaders struct {
	// CSP is the Content-Security-Policy, none when nil. Its frame-ancestors
	// also sets X-Frame-Options, for browsers that predate it.
	CSP CSP
	// Policy, when set, gives each request its policy in place of CSP, for
	// sources that depend on the request: starbase's script-src names the
	// request's own origin's /static/ and /c/, where 'self' would allow every
	// path of it. It runs on every request, and a nil policy sends none.
	// What it reads from the request, such as the Host header, is the
	// client's to choose: check it before it goes into the policy.
	Policy func(r *http.Request) CSP
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
```

SecurityHeaders sets the security headers on every response, and puts a fresh nonce for its CSP in each request's context (see Nonce). A handler can still change any of them for its own response.

#### func (SecurityHeaders) Handler {#SecurityHeaders.Handler}

```go
func (s SecurityHeaders) Handler(next http.Handler) http.Handler
```

Handler returns next behind the headers.

### type Sessions {#Sessions}

```go
type Sessions struct {
	// Name of the cookie, "sid" when empty. Over HTTPS it gets the __Host-
	// prefix, which binds the cookie to this host and Path=/.
	Name string
	// Secure is for a site served over HTTPS: the cookie is __Host- prefixed
	// and Secure. Without it, in development over plain http, the cookie has
	// its plain name.
	Secure bool
	// MaxAge is the cookie's lifetime, 400 days when zero (the longest
	// browsers keep one).
	MaxAge time.Duration
	// Accept, when set, lets a cookie carry an id that Sessions would not
	// make, such as one in an app's older format, so that a move to Sessions
	// signs no one out. It must check the format: the value is the client's.
	// New ids are 256-bit all the same.
	Accept func(id string) bool
}
```

Sessions hands every browser an opaque session id in a cookie: 256 random bits in base64url. It stores nothing, so a crawler's GET costs a Set-Cookie header and no row. What a session holds is the app's to store, through its writer, once the browser does something worth keeping.

#### func (Sessions) Clear {#Sessions.Clear}

```go
func (s Sessions) Clear(w http.ResponseWriter)
```

Clear deletes the cookie, for a logout.

#### func (Sessions) Ensure {#Sessions.Ensure}

```go
func (s Sessions) Ensure(w http.ResponseWriter, r *http.Request) string
```

Ensure returns the request's session id: the one Handler put in its context, else its cookie's, else a new one, which it sets as the cookie.

#### func (Sessions) Get {#Sessions.Get}

```go
func (s Sessions) Get(r *http.Request) (string, bool)
```

Get returns the session id the request's cookie carries, if it is one.

#### func (Sessions) Handler {#Sessions.Handler}

```go
func (s Sessions) Handler(next http.Handler) http.Handler
```

Handler puts the request's session id in its context, for SessionID, setting the cookie when the request had none.

#### func (Sessions) Renew {#Sessions.Renew}

```go
func (s Sessions) Renew(w http.ResponseWriter) string
```

Renew sets a new session id and returns it. Call it on login, so that an id planted before cannot carry over. The request's context keeps the old one.

### type Static {#Static}

```go
type Static struct {
	// contains filtered or unexported fields
}
```

Static serves the files of an fs.FS, and those Add gives it, under a URL prefix. URL names a file by its content, as in /static/site.3f2a9c1b2d4e.css, and that answer is immutable: browsers keep it for a year without asking again. A file asked for by its plain name, or by a hash that is no longer its content (a page from before a deploy, or a name with another number of hex digits, from 8 to 64), comes with no-cache and its ETag, so browsers ask each time and mostly get a 304. Text files go out precompressed (see Precompress), compressed in the background from the start (Warmed reports when that is done); other files go out as they are, with ranges. With Minify, its JavaScript and CSS go out minified.

#### func NewStatic {#NewStatic}

```go
func NewStatic(fsys fs.FS, prefix string, opts ...StaticOption) (*Static, error)
```

NewStatic reads every file of fsys, to serve them under prefix, such as "/static/", and minifies them with Minify's function. Mount it there, without http.StripPrefix. fsys may be nil, for a Static that serves only what Add gives it.

#### func (*Static) Add {#Static.Add}

```go
func (s *Static) Add(name string, data []byte)
```

Add serves data as the file name, in place of a file of that name. URL names the new content at once, and the URL of the content it replaces gets the new one with no-cache, like a hash from before a deploy. With Minify, it serves data minified. Add may run while s serves; s keeps data, which must not change after. It panics on a name that fs.ValidPath refuses.

#### func (*Static) ServeHTTP {#Static.ServeHTTP}

```go
func (s *Static) ServeHTTP(w http.ResponseWriter, r *http.Request)
```

#### func (*Static) URL {#Static.URL}

```go
func (s *Static) URL(name string) string
```

URL returns the hashed, immutable URL of the named file, or its plain URL when there is no such file.

#### func (*Static) Warmed {#Static.Warmed}

```go
func (s *Static) Warmed(context.Context) error
```

Warmed returns nil once the files NewStatic read are compressed, and an error until then, for a readiness check (Ready.Add("static", s.Warmed)): until then, a request for a file not compressed yet waits for it.

### type StaticOption {#StaticOption}

```go
type StaticOption func(*Static)
```

A StaticOption changes how NewStatic names or serves files.

#### func Minify {#Minify}

```go
func Minify(fn func(mediatype string, b []byte) ([]byte, error)) StaticOption
```

Minify has Static minify its JavaScript and CSS with fn: the files of fsys as NewStatic reads them, and those Add gives it. That happens before a file is named and compressed, so its URL, its ETag and its precompressed forms are all of the minified bytes. fn gets the media type, text/javascript or text/css, and the file as written, and returns it minified; lith/minify's Bytes is such a function, and so is the Bytes method of a tdewolff/minify M. A nil fn minifies nothing.

A file goes out as written when fn fails on it or saves less than a twentieth of it, and fn never sees a file named \*.min.js or \*.min.css: code that came minified, such as a vendored Datastar build, stays byte for byte the file its integrity hash was made from. Leave Minify out in development, for files that the browser's devtools show as written.

#### func StaticHashLen {#StaticHashLen}

```go
func StaticHashLen(n int) StaticOption
```

StaticHashLen puts n hex digits of a file's SHA-256 in the names URL makes, instead of 12. Names an app made the same way before, with that many digits, then stay current. It panics unless 8 \<= n \<= 64.

### type UploadLimits {#UploadLimits}

```go
type UploadLimits struct {
	Max     int64         // bytes; UploadMax
	MinRate int64         // bytes a second; UploadMinRate
	Window  time.Duration // UploadWindow
	Slack   time.Duration // UploadSlack
}
```

UploadLimits configure Upload. A zero field takes its default.
