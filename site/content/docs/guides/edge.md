---
title: The request edge
nav: The edge
headline: The request edge: sessions, CSRF, CSP and limits
summary: What happens to a request before its handler runs and after it has written: sessions, CSRF, security headers with a CSP nonce, client addresses, rate limits, compression, static files and health probes.
description: What happens around a handler: sessions, CSRF checks, security headers with a CSP nonce, client addresses, rate limits, compression and static files.
order: 8
---

`lith/edge` holds the middleware the apps share. It never touches the database: a session there is a cookie, and what a session holds is the app's to store, through its writer, once the browser does something worth keeping.

## The chain

The template builds the chain from the inside out, and keeps the probes outside it:

```go source=template/cmd/app/main.go
h := http.Handler(site.Routes())
h = edge.Sessions{Secure: *secure}.Handler(h)
h = edge.CSRF{Datastar: []string{"POST /cmd/", "POST /pace", "POST /ack"}}.Handler(h)
h = edge.SecurityHeaders{CSP: edge.DefaultCSP(), HSTS: *secure}.Handler(h)
h = edge.Compress(h)
h = edge.Recover(log, h)
h = edge.LogRequests(log, h)
```

Each layer's place has a reason:

- `Sessions` sits inside `CSRF`, so a request that `CSRF` refuses gets no cookie.
- `Recover` sits around `Compress`: a panic drops the compressed body held so far, and the client gets a clean 500.
- `LogRequests` sits around `Recover`, so it logs the 500 a panic became.
- `/healthz` and `/readyz` sit outside the chain: a probe gets no cookie and makes no log line.
- `/static/` sits outside `Sessions`, behind the headers, `Recover` and the log: its answers are public and immutable, and a session cookie on one would go to everyone a shared cache hands it to.

```go source=template/cmd/app/main.go
static := edge.SecurityHeaders{CSP: edge.DefaultCSP(), HSTS: *secure}.Handler(site.Static())
static = edge.LogRequests(log, edge.Recover(log, static))
```

The server has no `WriteTimeout`: each write to a stream has a deadline of its own, and a server-wide timeout would cut every stream after that long. Its `ReadTimeout` gives a request's headers and body 15 s on every route, the probes and the static files too, and leaves streams alone ([Request bodies](#request-bodies)).

```go source=template/cmd/app/main.go
srv := &http.Server{
	Handler:           mux,
	ReadHeaderTimeout: 10 * time.Second,
	ReadTimeout:       15 * time.Second,
	IdleTimeout:       2 * time.Minute,
	ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	// ReadTimeout ends a body that does not arrive, on every route, read
	// or not, and leaves streams alone; an upload extends it with
	// edge.Upload. No WriteTimeout: a stream gives each of its
	// writes a deadline.
}
```

## Sessions

`edge.Sessions` hands every browser an opaque session id in a cookie, 256 random bits in base64url, and stores nothing. A crawler's GET costs a `Set-Cookie` header and no row. A handler reads the id with `edge.SessionID(r.Context())`, and the app writes a row keyed by it with the first command that needs one: the board's tab rows and preferences.

The cookie is `HttpOnly`, `SameSite=Lax`, and lives 400 days, the longest a browser keeps one (`MaxAge`). With `Secure: true`, for a site served over HTTPS, it is `Secure` and named `__Host-sid`, which binds it to the host and to `Path=/`. In development over plain HTTP it is `sid`.

- `Renew(w)` sets a new id; call it at login, so that an id planted before cannot carry over.
- `Clear(w)` deletes the cookie, for a logout.
- `Ensure(w, r)` returns the request's id, making one when it has none, for a handler outside `Handler`.
- `Accept`, a function, lets a cookie carry an id in an app's older format, so a move to `Sessions` signs no one out. It must check the format: the value is the client's.

A session id is an identifier, not a limiter key. `Sessions` signs nothing and keeps no record, so any well-formed id is valid, and a request without a cookie gets a new one: a client that sends no cookie is a new session with every request. A limit or a "one per session" rule keyed by it alone is one a client steps around; put a limit by the client's address ([below](#rate-limits)) in front of it. Guessing and fixation are not at issue: 256 random bits, `__Host-`, and `Renew` at login.

## Cross-site requests

`edge.CSRF` refuses unsafe requests, every method but GET, HEAD and OPTIONS, that come from another site, with a 403:

- A request passes when the browser's `Sec-Fetch-Site` header says `same-origin`, or `none` (the user typed the address).
- Without that header, as from browsers older than 2023, its `Origin` must name this host.
- A request with neither header is refused: a client that is not a browser sends an `Origin`.
- A same-site request from a sibling subdomain passes only if `Origins` lists it.

`Datastar` lists the action routes, such as `POST /cmd/`, and the template's `POST /pace` and `POST /ack` ([Streams](/docs/guides/streams#pacing-a-stream-by-the-page)), whose requests must also carry `Datastar-Request: true`. Datastar sends that header with every action; an HTML form cannot send it, and a cross-site `fetch` only after a CORS preflight, which the app does not allow. `Exempt` lists routes that skip the check, such as `POST /hooks/{name}` for webhooks, which authenticate with a signature instead. Every route that changes data passes the check, or is in `Exempt` for a reason.

## Security headers and the nonce

`edge.SecurityHeaders` sets the headers on every response: the Content Security Policy, `X-Content-Type-Options: nosniff`, `Referrer-Policy` (`strict-origin-when-cross-origin` unless set), `Cross-Origin-Opener-Policy: same-origin`, `X-Frame-Options` from the policy's `frame-ancestors`, and with `HSTS` a two-year `Strict-Transport-Security` that includes subdomains (`HSTSNoSubdomains` leaves them out). `Extra` adds headers such as `Permissions-Policy`.

`edge.DefaultCSP()` is a policy for a Datastar app:

| Directive | Sources |
| --- | --- |
| `default-src`, `connect-src`, `font-src`, `base-uri`, `form-action` | `'self'` |
| `script-src` | `'self'`, the request's nonce, `'unsafe-eval'` |
| `style-src` | `'self'`, `'unsafe-inline'` |
| `img-src` | `'self'`, `data:` |
| `object-src`, `frame-ancestors` | `'none'` |

`'unsafe-eval'` is required: Datastar compiles its expressions with `Function`. Without it a page loads, shows its server-rendered HTML, and does nothing, and no error says why. Inline styles are allowed for components that render `<style>` into their shadow roots.

`SecurityHeaders` makes a new nonce for every request and puts it in the request's context, where `edge.Nonce(ctx)` finds it. An inline script, such as an import map, carries it. This site's pages get it from `edge.Nonce(r.Context())` and render their import map with it:

```go source=site/internal/views/shell.go
// importMap is the page's import map, with the request's nonce.
func importMap(s Shell) templ.Component {
	return templ.Raw(`<script type="importmap" nonce="` + s.Nonce + `">` + s.Assets.ImportMap + `</script>`)
}
```

A policy that depends on the request, such as one that names the request's own origin, comes from `Policy func(r *http.Request) edge.CSP` instead of `CSP`. What it reads from the request is the client's to choose: check it first.

## Client addresses

`edge.ClientIP(r)` returns the address of the client behind the local reverse proxy. Only a peer on loopback or a Unix socket counts as the proxy; from any other peer, `X-Forwarded-For` and `X-Real-IP` are ignored, since a client can send them itself to dodge its rate limit or to spend someone else's. From the proxy, the address is the last one of `X-Forwarded-For`, and `X-Real-IP` counts only without it: a proxy may pass on an `X-Real-IP` the client sent. The template's Caddy block replaces both:

```text source=template/deploy/app.caddy
header_up X-Forwarded-For {http.request.remote.host}
header_up X-Real-IP {http.request.remote.host}
```

`edge.ClientKey(r)` is the address as a rate-limit key: an IPv6 client counts by its /64, since one subscriber holds a whole /64 and could rotate through it.

Whoever can connect to the app's listener from loopback or through its socket can name any address. On a loopback port that is every local user of the host, another app's compromised service user too. The template listens on a Unix socket of mode 0660 in a directory of mode 0750, both of the app's group, which Caddy's user joins, so only Caddy gets in ([Deploying](/docs/guides/deploying#caddy)). A proxy that connects from another address, such as a container bridge, is not believed at all: every client then counts as the proxy, and one abuser spends the limit of all. `edge.TrustedProxies(ln.Addr())` says which case a listener is, and the template logs it at start as `proxy_headers_from`.

## Slow links

`edge.SlowLink(r)` reports whether a request says its client is on a slow link: the `Save-Data: on` header, which a browser sends when its user asked to save data, and a lith page sends on its stream's request when it measured a slow link as it started or fell behind its stream later (`live.NewestFrame`); Chromium's client hints `ECT` (slow-2g, 2g or 3g), `RTT` (over 300 ms) and `Downlink` (under 0.8 Mbit/s); or the query parameter `net=slow`, which pages put in their stream's URL before they sent `Save-Data`, so that tabs still open from then keep their slow stream. Chromium sends those hints once a document asked for them with `Accept-CH`, whose value is `edge.NetworkHints`. A stream handler passes the answer to `live.Spec.Slow` ([Streams: slow links](/docs/guides/streams#slow-links)).

## Rate limits

`edge.Limiter` limits what each key may take, in three shapes:

| Constructor | Allows | For |
| --- | --- | --- |
| `NewLimiter(rate, burst)` | `burst` at once, then `rate` a second | actions: a token bucket that refills as time passes |
| `NewWindow(n, per)` | `n` per window of `per`, the window opening with the first use | coarse caps, up to `2n` around a window's edge |
| `NewSliding(n, per)` | `n` in any span of `per` | a promise to people, such as five messages an hour |

The board limits actions per client address:

```go source=site/internal/docs/example/web/server.go
limit: edge.NewLimiter(5, 20), // per client address: 20 actions at once, then 5 a second
```

```go source=site/internal/docs/example/web/server.go
if !s.limit.Allow(edge.ClientKey(r), 1, time.Now()) {
	s.notify(r.Context(), a, "Too many actions at once. Wait a moment, then try again.")
	w.WriteHeader(http.StatusTooManyRequests)
	return a, false
}
```

The 429 is for the page's retry; the person reads the notice on the tab's stream ([Commands](/docs/guides/commands#rate-limits-and-quotas)).

Pass `time.Now()` to `Allow`: its monotonic reading keeps a step of the wall clock from refilling a bucket or resetting a window. A key a client chooses, such as its session id, costs it nothing to change, so check the per-address limiter first, and only requests it lets through make buckets in a per-session one. A limiter sweeps buckets that filled up again as it goes, so its memory holds the keys limited lately.

## Compression

`edge.Compress` compresses the answers to GET, HEAD and QUERY (`@query`, a read like GET) with zstd, brotli or gzip, the one the request accepts best, zstd first when it accepts them equally: zstd compresses a page in about an eighth of brotli's time, for about a sixth more bytes. Safari accepts zstd from 26.3, and older Safari gets brotli. It leaves alone event streams, which compress themselves, answers that set their own `Content-Encoding`, partial content, types that do not compress, bodies under 512 bytes, and answers with `Cache-Control: no-transform`. Other methods pass untouched. Caddy does not compress (`encode` stays out of the site block): the app compresses everything itself.

A compressed page that holds a secret of the user beside text the request reflected leaks the secret to an attacker who can make the browser send requests and read their sizes (BREACH). lith's pages hold none: the session lives in the cookie, and the CSRF check reads headers. A page that renders such a secret, a token or an address next to a search term, sends `Cache-Control: no-transform`, and goes out uncompressed.

## Request bodies

`sse.ReadSignals` and `ReadSignalsLimit` cap an action's body (1 MiB, the template's 8 KiB), and give it `sse.SignalsTimeout`, 10 s, to arrive: they set a read deadline on the connection through `http.ResponseController` before they read, and clear it once the body is in. A client that declares a body and sends one byte of it then gets an error, and the server closes the connection, where it held a goroutine until the client left.

A route that never reads its body needs the same: `/pace`, `/ack`, a page. Over HTTP/1.1 Go's server reads what is left of a body before it sends the answer, so without a deadline a client that declares 100 KB and sends a byte every few seconds held the connection and a goroutine as long as it liked, and never got the answer. The server's `ReadTimeout`, 15 s in the template and the site, is that deadline, for the headers and the body together, on every route. Once it passes, the server gives up on the body, sends the answer and closes the connection.

It leaves streams alone. v0.5.0's docs said a server-wide `ReadTimeout` would end every stream, and that Caddy's `read_body` would; neither does. Over HTTP/1.1 Go's server clears the read deadline once the body has been read to its end, and before the handler runs for a request without one, as it starts to watch the connection for a client that leaves. Over HTTP/2 the timeout is a timer per stream, which does nothing once the body has ended. So a stream's `@get`, and a `@post` stream whose signals are in, run on as long as they like (`TestReadTimeoutSparesStreams` in `edge`, and the template's `TestHalfSentBody`).

A handler that takes a large body, such as an upload, wraps it before the first read with `edge.Upload(w, r, edge.UploadLimits{Max, MinRate, Window, Slack})`, and assigns the reader to `r.Body` (`edge.UploadBody(w, r, minRate, maxSize)` is the same with the default window and slack). The body gets a `Window` (10 s), and each time it has sent `MinRate` times `Window` bytes, a `Window` from then, up to `Max/MinRate` plus `Slack` (1 min) in all; `http.MaxBytesReader` caps it at `Max`, and `edge.TooLarge(err)` finds that error through `ParseMultipartForm`, a part of `MultipartReader` and a `json.Decoder`. So a body that falls below the rate is cut off a `Window` after it last kept up, a burst before a stall too. It works with `ParseMultipartForm`, `MultipartReader` and plain reads, over HTTP/1.1 and HTTP/2. A fixed deadline does not fit a slow link: 10 MB at 0.4 Mbit/s takes 200 s. `UploadMinRate` is 8 KiB/s: three uploads that share a 0.4 Mbit/s link get some 133 kbit/s each, about 16 KB/s, twice that. A server with a `WriteTimeout` would fail the answer to an upload that took longer, so `Upload` clears the write deadline while the body arrives and sets it to the server's `WriteTimeout` from the body's end. It sets the deadlines through `http.ResponseController`, so the writer must unwrap to the server's (lith's `Compress` and `LogRequests` do; an app's own response wrapper needs an `Unwrap() http.ResponseWriter` method); when one cannot be set, `Upload` returns an error that wraps `http.ErrNotSupported`, beside a body still capped at `Max`. Whatever sets a read deadline by hand: set or extend it before the first `Read`, never after the body's end. Over HTTP/1.1 the server then watches the connection for a client that leaves, and a deadline set on it lands on that watch and cancels the request's context when it passes. Clearing it with the zero time is harmless, which is what `sse.ReadSignals` does once a body is in.

Behind Caddy, a client that trickles its body still holds Caddy's connection to the client for as long as Caddy allows. Caddy passes a body on to the app as it arrives and keeps the client's connection open, without the answer, while the client trickles: measured with Caddy 2.10.2 over HTTP/1.1, a client that sent a byte every few seconds still held it after 40 s, while the app had closed its side at its deadline. Since Caddy 2.11.5, `read_body_idle` (1 min by default, reset by every read) closes a client that stops sending altogether; only one that keeps trickling holds Caddy's side. The defence against a trickle belongs in the app: `ReadTimeout` by default, and `Upload` in each upload's handler. Caddy has none that fits. Its `read_body` timeout does not end streams (Caddy is Go's server as well: a stream through Caddy with `read_body 5s` ran its full 25 s), but it is global, for every site on the host, and since Caddy 2.11.5 it also caps Caddy's own idle deadline per read (`read_body_idle`, 1 min by default) on every request: a short value cuts every upload that takes longer, and nothing per route lifts it. So on Caddy 2.11.5 and later it is at most a ceiling sized to the host's slowest honest upload, not a defence against a trickle. The per-route `request_body { read_timeout }` of Caddy 2.10 no longer exists since 2.11.5; `request_body { max_size }` on an upload's route limits its size, not its time. `min_rate` is global too, and averaged from the start of the body, so it would also cut long-lived WebSockets over HTTP/2 on other sites, which read through the request body. HTTP/3 has no `read_body` at all. lith recommends none of these and leaves Caddy's timeouts to the host. The template's Caddy block caps bodies at 1 MB (`request_body { max_size 1MB }`).

## Static files

`edge.NewStatic(fsys, "/static/")` serves an `fs.FS` under a prefix. `URL(name)` names a file by its content, as in `/static/style.3f2a9c1b2d4e.css`, and that answer is immutable: browsers keep it a year without asking again. A file asked for by its plain name, or by a hash from before a deploy, comes with `no-cache` and its ETag, so browsers ask each time and mostly get a 304. Text files go out precompressed, brotli at level 11 and gzip at 9, compressed once in the background when the process starts.

```go source=template/internal/web/server.go
var opts []edge.StaticOption
if !dev {
	opts = append(opts, edge.Minify(minify.Bytes))
}
static, err := edge.NewStatic(files, "/static/", opts...)
if err != nil {
	return nil, err
}
```

`edge.Minify(fn)` has Static minify its JavaScript and CSS before it names and compresses them, so the hashed URL, the ETag and the brotli and gzip copies are all of the minified bytes. `minify.Bytes` comes from the module `github.com/zweiundeins/lith/minify`, on esbuild for JavaScript and tdewolff/minify for CSS: an app opts in by importing it, and one that does not pulls in nothing. A file the function fails on, or makes less than a twentieth smaller, goes out as written, and the function never sees a file named `*.min.js` or `*.min.css`. That keeps vendored builds byte for byte what their integrity hash was made from: esbuild takes 147 bytes, 0.2%, off the 68 KB Datastar build, and a page that pins the build, as this site does, would no longer load it. The template and this site leave the option out with `-dev`, so the browser's devtools show the files as written.

Minified, the template's stylesheet takes 514 bytes under brotli instead of 569, and this site's CSS bundle 23.4 KB instead of 34.4. `newest-frame.js` comes as a minified build, 1,501 bytes under brotli, which esbuild makes 1.3% smaller, so it goes out as vendored. Minifying at startup costs `NewStatic` about 10 ms on the template's files, 9 of them for the Datastar build that then goes out as written, and the module adds about 4 MiB to the template's stripped binary, most of it esbuild. `bench/RESULTS.md` has the numbers from when the module minified JavaScript with tdewolff/minify, which took half the time and 3.4 MiB less, but dropped side effects after `void` of a logical expression.

`Add(name, data)` serves generated bytes as a file, such as a CSS bundle built at startup, minified like the rest; this site serves its stylesheet that way. Precompression keeps every content in memory for the life of the process, and brotli at level 11 needs about 85 MB of heap for a moment to compress a 1.1 MB file, which the Go runtime then keeps: this site measured it with a chart library, and vendors none.

### JavaScript from a Go module

A Go module that has a browser side, such as a co-editing layer with its editor's protocol in ES modules, can ship that JavaScript in the module itself, with no vendor command. The app's esbuild script asks Go where the module lives and aliases its folder:

```js
const dir = execFileSync('go', ['list', '-m', '-f', '{{.Dir}}', 'example.com/coedit'], { encoding: 'utf8' }).trim()
await build({
	entryPoints: ['src/editor.js'], bundle: true, format: 'esm',
	alias: {
		coedit: join(dir, 'editor'),
		'prosemirror-model': join(here, 'node_modules/prosemirror-model'), // and each other peer package
	},
})
```

`go list` names the version that go.mod requires, in the module cache, or the local checkout a `replace` directive points to, so the JavaScript always matches the Go code it talks to. Alias every package the module's JavaScript shares with the app to the app's own `node_modules`, and pin the same versions in the app's `package.json`: esbuild resolves an import from the importing file's folder, so it would take the copy in a checkout's own `node_modules` and bundle the package twice, which breaks `instanceof` across the copies. The bundle goes into the app's static folder like any other file.

## Health and readiness

`edge.Health(version)` answers `200 ok <version>` while the process serves, and checks nothing: a busy database never fails it. The deploy script waits for it, and compares the version. `edge.Ready` runs the checks an app registers, in parallel, within two seconds, and answers 503 naming the ones that failed:

```go source=template/cmd/app/main.go
ready := &edge.Ready{Log: log}
ready.Add("database", func(ctx context.Context) error {
	var one int
	return db.R.QueryRowContext(ctx, "SELECT 1").Scan(&one)
})
```

A check must not queue behind the writer: a command waits for every batch ahead of it, which turns a busy writer into a failed probe. Read from the read pool, or look at state in memory, as this site does for its writer and hub. `edge.Static.Warmed` is such a check: it passes once the static files are compressed, on two goroutines from the start, and until then a request for a file not compressed yet waits for it. The template and this site register it as `static`.

## Errors and logs

`edge.Recover(log, h)` turns a panic into a 500, logged with its stack; when the answer has begun, it aborts the connection instead, so the client can tell that the answer is incomplete. `edge.LogRequests(log, h)` logs a line per request as it ends: method, path, status, bytes, duration and client, at Error for a 5xx and at Info otherwise. A handler whose requests come several times a second from one page calls `edge.Quiet(r)`, and their lines go out at Debug unless they fail: the cursor party's moves do, 15 a second from every pointer.
