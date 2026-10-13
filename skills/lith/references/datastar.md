# Datastar gotchas shared by the lith projects

Each entry cost a project time. The project in brackets has the test or the
write-up; look there before changing the behaviour.

## Attributes and signals

- Attribute syntax is `data-<plugin>:<key>__<modifier>`: `data-on:submit__prevent`,
  `data-bind:email`. Hyphenated forms parse as an unknown plugin and are ignored
  without an error, so nothing binds and a form falls back to a native GET.
  [swisscyberguard CLAUDE.md, Stack]
- HTML lowercases attribute names: `data-bind:fullFare` binds a second, empty
  signal `fullfare`. Use kebab-case keys. [pendlerpreis docs/03-findings.md]
- `data-on:load` listens for its element's own `load` event, which only
  elements that load something fire (img, iframe, script, link). On a div or
  the body it never runs: the page's load event goes to window. Open streams
  with `data-init`. [pendlerpreis; swisscyberguard's two streams and its
  credential auto-hide sat on divs and never ran]
- `data-init__delay` starts a timer that is never cleared: it fires even
  after its element is gone, and a leftover hid a later reveal early. A
  timeout that must end with its element uses `data-on-interval__duration`,
  which Datastar clears when the element goes. [swisscyberguard CRED-01]
- A regex in `filterSignals` needs its slashes; without them the whole handler
  fails to compile. [pendlerpreis]
- Signals that start with `_` stay in the browser and are not sent with
  actions. Use them for view-only and ephemeral state (`$_pending`, `$_size`),
  and for anything that must never reach the server (a password field).
  [hyperlith README, swisscyberguard]
- Assigning an object to a signal merges into it; `null` deletes the signal and
  leaves every effect that read it stale. Use `false` or `''`. [starbase CLAUDE.md]
- `data-bind` on a custom element only writes to a signal that exists: declare
  it with `data-signals` first. [starbase]
- Copy that ends up inside an expression is a single-quoted JS string: a
  straight apostrophe in the copy closes it and throws. [swisscyberguard]

## Morphs and streams

- Inputs inside a patched region need `data-ignore-morph` and their value from
  a signal, or the next frame resets what someone is typing. `<details>` needs
  `data-preserve-attr="open"`, and hosts whose attributes come from signals
  need `data-preserve-attr` for those attributes. [kanban, starbase]
- Datastar 1.0.4 retries a request by its `retry` option. `'auto'` (the
  default) retries network errors only: it gives up at a proxy's 502 and
  never reopens a stream the server ended cleanly, so a deploy leaves pages
  silently stale. `'error'` also retries 4xx and 5xx answers. `'always'` also
  reopens a stream that ended cleanly, one the server ended on purpose
  included, and keeps a full server's 503s coming. A 204 stops every mode.
  [libretto, kanban]
- Open a stream with `data-init` on an element with an id and
  `retry: 'error', retryMaxCount: Infinity`, and pass the id as
  `live.Spec.Reconnect`. The server decides when the page reconnects: on a
  deploy the hub ends each stream with `sse.Reconnect`, which re-creates that
  element so its data-init runs again with its own options, and `'error'`
  carries the request through the restart. A stream the server ends otherwise
  stays closed. Refuse a stream for good with 204. [lith; kanban's e2e test
  restarts the server under an open board]
- The element that `Spec.Reconnect` names does nothing but open the stream:
  every deploy runs its data-init again, so a tab id or a signal made there
  would change. [2und1]
- A watchdog on the page needs the server's ping. An SSE comment never reaches
  the page; `live.Options.HeartbeatSignal` sends the heartbeat as a signal the
  watchdog can read. [kanban]
- Datastar cancels an in-flight request to the same URL when a new one starts.
  Actions that must all arrive (painting, moves) use
  `requestCancellation: 'disabled'`. [starbase, kanban]
- Keyed elements that contain Rocket components crash Datastar 1.0.4's morph
  (`Maximum call stack`, upstream #1209). Use Starbase's patched build until a
  release has the fix. [starbase, kanban]
- Expressions on many repeated elements cost compile time and markup per
  frame (200 cards times a few expressions). Put the behaviour on the
  container and delegate. [kanban]
- An action's response carries nothing the view needs: the outcome arrives on
  the tab's stream. Patching elements from an action fights the next frame,
  which overwrites them. Signals from an action are fine (validation
  messages). [hyperlith README, kanban]

## Security headers

- Datastar compiles expressions with `Function()`: the CSP needs
  `'unsafe-eval'`. The violation is not a console error, so a page can look
  fine and do nothing. [pendlerpreis, swisscyberguard]
- An inline `<script>` needs the request's CSP nonce. [starbase]

## Testing

- "The server sends the right bytes" and "the page works" are different
  claims. A Go test that reads the stream over HTTP proves the first only.
  Drive the page in a real browser for the second: starbase's Chrome tests,
  kanban's e2e harness, pendlerpreis' `tools/capture`. [pendlerpreis]
