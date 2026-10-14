---
title: The request edge
nav: The edge
headline: The request edge: sessions, CSRF, CSP and limits
summary: What happens to a request before its handler runs and after it has written: sessions, CSRF, security headers with a CSP nonce, client addresses, rate limits, compression, static files and health probes.
description: What happens around a handler: sessions, CSRF checks, security headers with a CSP nonce, client addresses, rate limits, compression and static files.
order: 7
---

`lith/edge` holds the middleware the apps share. It never touches the database: a session there is a cookie, and what a session holds is the app's to store, through its writer, once the browser does something worth keeping.

## The chain

The template builds the chain from the inside out, and keeps the probes outside it:

```go source=template/cmd/app/main.go
h := http.Handler(site.Routes())
h = edge.Sessions{Secure: *secure}.Handler(h)
h = edge.CSRF{Datastar: []string{"POST /cmd/"}}.Handler(h)
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

The server has no `WriteTimeout`: each write to a stream has a deadline of its own, and a server-wide timeout would cut every stream after that long.

```go source=template/cmd/app/main.go
srv := &http.Server{
	Handler:           mux,
	ReadHeaderTimeout: 10 * time.Second,
	IdleTimeout:       2 * time.Minute,
	ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	// No WriteTimeout: a stream gives each of its writes a deadline.
}
```

## Sessions

`edge.Sessions` hands every browser an opaque session id in a cookie, 256 random bits in base64url, and stores nothing. A crawler's GET costs a `Set-Cookie` header and no row. A handler reads the id with `edge.SessionID(r.Context())`, and the app writes a row keyed by it with the first command that needs one: the board's tab rows and preferences.

The cookie is `HttpOnly`, `SameSite=Lax`, and lives 400 days, the longest a browser keeps one (`MaxAge`). With `Secure: true`, for a site served over HTTPS, it is `Secure` and named `__Host-sid`, which binds it to the host and to `Path=/`. In development over plain HTTP it is `sid`.

- `Renew(w)` sets a new id; call it at login, so that an id planted before cannot carry over.
- `Clear(w)` deletes the cookie, for a logout.
- `Ensure(w, r)` returns the request's id, making one when it has none, for a handler outside `Handler`.
- `Accept`, a function, lets a cookie carry an id in an app's older format, so a move to `Sessions` signs no one out. It must check the format: the value is the client's.

## Cross-site requests

`edge.CSRF` refuses unsafe requests, every method but GET, HEAD and OPTIONS, that come from another site, with a 403:

- A request passes when the browser's `Sec-Fetch-Site` header says `same-origin`, or `none` (the user typed the address).
- Without that header, as from browsers older than 2023, its `Origin` must name this host.
- A request with neither header is refused: a client that is not a browser sends an `Origin`.
- A same-site request from a sibling subdomain passes only if `Origins` lists it.

`Datastar` lists the action routes, such as `POST /cmd/`, whose requests must also carry `Datastar-Request: true`. Datastar sends that header with every action; an HTML form cannot send it, and a cross-site `fetch` only after a CORS preflight, which the app does not allow. `Exempt` lists routes that skip the check, such as `POST /hooks/{name}` for webhooks, which authenticate with a signature instead. Every route that changes data passes the check, or is in `Exempt` for a reason.

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

```text source=template/deploy/Caddyfile
header_up X-Forwarded-For {http.request.remote.host}
header_up X-Real-IP {http.request.remote.host}
```

`edge.ClientKey(r)` is the address as a rate-limit key: an IPv6 client counts by its /64, since one subscriber holds a whole /64 and could rotate through it.

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
	http.Error(w, "too many actions, wait a moment", http.StatusTooManyRequests)
	return a, false
}
```

Pass `time.Now()` to `Allow`: its monotonic reading keeps a step of the wall clock from refilling a bucket or resetting a window. A key a client chooses, such as its session id, costs it nothing to change, so check the per-address limiter first, and only requests it lets through make buckets in a per-session one. A limiter sweeps buckets that filled up again as it goes, so its memory holds the keys limited lately.

## Compression

`edge.Compress` compresses the answers to GET, HEAD and QUERY (`@query`, a read like GET) with brotli, zstd or gzip, the one the request accepts best, brotli first when it accepts them equally. It leaves alone event streams, which compress themselves, answers that set their own `Content-Encoding`, partial content, types that do not compress, and bodies under 512 bytes. Other methods pass untouched. Caddy does not compress (`encode` stays out of the site block): the app compresses everything itself.

## Static files

`edge.NewStatic(fsys, "/static/")` serves an `fs.FS` under a prefix. `URL(name)` names a file by its content, as in `/static/style.3f2a9c1b2d4e.css`, and that answer is immutable: browsers keep it a year without asking again. A file asked for by its plain name, or by a hash from before a deploy, comes with `no-cache` and its ETag, so browsers ask each time and mostly get a 304. Text files go out precompressed, brotli at level 11 and gzip at 9, compressed once in the background when the process starts.

```go source=template/internal/web/server.go
static, err := edge.NewStatic(files, "/static/")
if err != nil {
	return nil, err
}
```

`Add(name, data)` serves generated bytes as a file, such as a CSS bundle built at startup; this site serves its stylesheet that way. Precompression keeps every content in memory for the life of the process, and brotli at level 11 needs about 85 MB of heap for a moment to compress a 1.1 MB file, which the Go runtime then keeps: this site measured it with a chart library, and vendors none.

## Health and readiness

`edge.Health(version)` answers `200 ok <version>` while the process serves, and checks nothing: a busy database never fails it. The deploy script waits for it, and compares the version. `edge.Ready` runs the checks an app registers, in parallel, within two seconds, and answers 503 naming the ones that failed:

```go source=template/cmd/app/main.go
ready := &edge.Ready{Log: log}
ready.Add("database", func(ctx context.Context) error {
	var one int
	return db.R.QueryRowContext(ctx, "SELECT 1").Scan(&one)
})
```

A check must not queue behind the writer: a command waits for every batch ahead of it, which turns a busy writer into a failed probe. Read from the read pool, or look at state in memory, as this site does for its writer and hub.

## Errors and logs

`edge.Recover(log, h)` turns a panic into a 500, logged with its stack; when the answer has begun, it aborts the connection instead, so the client can tell that the answer is incomplete. `edge.LogRequests(log, h)` logs a line per request as it ends: method, path, status, bytes, duration and client, at Error for a 5xx and at Info otherwise. A handler whose requests come several times a second from one page calls `edge.Quiet(r)`, and their lines go out at Debug unless they fail: the cursor party's moves do, 15 a second from every pointer.
