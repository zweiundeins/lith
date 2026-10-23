# Datastar gotchas in lith apps

Each entry cost an app time. Where the brackets name lith, its site, kanban
or starbase (both public repos), that code has the test or the write-up;
look there before changing the behaviour.

## Attributes and signals

- Attribute syntax is `data-<plugin>:<key>__<modifier>`: `data-on:submit__prevent`,
  `data-bind:email`. Hyphenated forms parse as an unknown plugin and are ignored
  without an error, so nothing binds and a form falls back to a native GET.
- HTML lowercases attribute names: `data-bind:fullFare` binds a second, empty
  signal `fullfare`. Use kebab-case keys.
- `data-on:load` listens for its element's own `load` event, which only
  elements that load something fire (img, iframe, script, link). On a div or
  the body it never runs: the page's load event goes to window. Open streams
  with `data-init`.
- `data-init__delay` starts a timer that is never cleared: it fires even
  after its element is gone, and a leftover hid a later reveal early. A
  timeout that must end with its element uses `data-on-interval__duration`,
  which Datastar clears when the element goes.
- A regex in `filterSignals` needs its slashes; without them the whole handler
  fails to compile.
- Signals that start with `_` stay in the browser and are not sent with
  actions. Use them for view-only and ephemeral state (`$_pending`, `$_size`),
  and for anything that must never reach the server (a password field).
  [hyperlith README]
- Assigning an object to a signal merges into it; `null` deletes the signal and
  leaves every effect that read it stale. Use `false` or `''`. [starbase CLAUDE.md]
- An effect that writes a signal undoes any other write to that signal at
  once, a signal patch from the server included: one writer per signal,
  and react to patches with `data-on-signal-patch`. [tickit]
- Reading a missing key creates it: `$_outcomes[op]` on an unknown op adds
  the key with `''`. Test for `''`, not `undefined`. [tickit]
- `@post` (any action) returns a promise in Datastar 1.0.4, undocumented: it
  settles after `finished` and rejects after `retries-failed`. lith's
  template and recipes do not rely on it (they follow `datastar-fetch`
  events); an app that chains on it catches the rejection. [tickit]
- `data-bind` on a custom element only writes to a signal that exists: declare
  it with `data-signals` first. [starbase]
- Copy that ends up inside an expression is a single-quoted JS string: a
  straight apostrophe in the copy closes it and throws.

## Morphs and streams

- Inputs inside a patched region need `data-ignore-morph` and their value from
  a signal, or the next frame resets what someone is typing. `<details>` needs
  `data-preserve-attr="open"`, and hosts whose attributes come from signals
  need `data-preserve-attr` for those attributes. [kanban, starbase]
- Datastar 1.0.4 retries a request by its `retry` option. `'auto'` (the
  default) retries network errors only: it gives up at a proxy's 502 and
  never reopens a stream the server ended cleanly, so a deploy leaves pages
  silently stale. `'error'` also retries every answer from 400 to 599:
  a stream that gets 400, 401 or 429 asks again without end, firing
  `retrying` and `error` each time. `'always'` also reopens a stream that
  ended cleanly, one the server ended on purpose included, and keeps a full
  server's 503s coming. A 204 or a 3xx stops every mode. A stream whose page
  must tell the person something (a session that ended) opens and renders
  it from a view instead of answering 401. A tab that hides closes its
  `@get` stream and opens it again when it shows, without data-init and
  without `retrying`: listen for `visibilitychange` too. [kanban, lith]
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
  would change.
- Markup that `Spec.Refresh` morphs in after a deploy: a plain
  `data-signals` overwrites the page's values (the tab id, pending ops), so
  declare signals there with `data-signals:x__ifmissing`, or keep them
  outside the container. An element whose attributes change runs its
  `data-init` again; the stream's element inside the container would open a
  second stream. Report the build as a signal, not in the URL the expression
  writes: Datastar aborts the request in flight only for a new one to the
  same URL, which it takes before it adds the signals.
- Datastar 1.0.4 builds a `@get`'s headers once, as the expression runs. A
  reopen it makes by itself (a retry, a hidden tab shown again) sends the
  same headers but reads the signals afresh into the `datastar` parameter.
  So report what changes (build, typed flag, assets version) as signals,
  `filterSignals: {include: /^_(build|dirty)$/, exclude: /(?!)/}` (the
  default `exclude` drops every `_` signal, even one `include` names), and
  read them with `sse.ReadSignals`. A build in a header refreshed a hidden
  tab on every show after a deploy, and a typed flag in one stayed false.
  Only what never changes for the page, its path, may stay a header. [lith,
  2und1.ch]
- A watchdog on the page needs the server's ping. An SSE comment never reaches
  the page; `live.Options.HeartbeatSignal` sends the heartbeat as a signal the
  watchdog can read. [kanban]
- Datastar 1.0.4 cancels an in-flight request with the same method to the
  same URL when a new one starts, from any element of the page. Actions that
  must all arrive (painting, moves, every op) use
  `requestCancellation: 'disabled'`. [starbase, kanban]
- With `contentType: 'form'`, a failed `checkValidity` ends the request
  after `started` without `finished`: an op's in-flight count never drops.
  Such a form gets `novalidate` and checks its fields in the expression.
- Keyed elements that contain Rocket components crash Datastar 1.0.4's morph
  (`Maximum call stack`, upstream #1209). Use Starbase's patched build until a
  release has the fix. [starbase, kanban]
- Expressions on many repeated elements cost compile time and markup per
  frame (200 cards times a few expressions). Put the behaviour on the
  container and delegate. [kanban]
- A patch with `useViewTransition` (`sse.Patch{ViewTransition: true}`) runs a
  transition of the whole document, and while it runs Chromium gives every
  press on the page to `<html>`: buttons and links miss their clicks. Scope it
  with `ViewTransitionSelector` to the element that holds what moves; Datastar
  then calls that element's `startViewTransition`, and the rest of the page
  keeps working. Inside the scope nothing takes a press until it ends: it
  goes to what lies beneath. A browser without scoped transitions morphs at
  once. [lith's site]
- Datastar sends signals (or the `payload`) as the `datastar` query
  parameter for `@get` and `@delete`, and as a JSON body for every other
  verb, `@query` included. Read them with `sse.ReadSignals(w, r, &v)` for any
  method, before `sse.Open` (its headers discard the body:
  `sse.ErrBodyConsumed`). `@get` for streams and reads whose signals are all
  `_`, `@query` for reads that send real signals (searches, filters), `@post`
  for commands. [lith]
- `openWhenHidden` defaults to false for `@get` only: a `@get` stream closes
  while its tab is hidden and reopens when it shows, every other verb stays
  open. A stream opened with `@query` sets `openWhenHidden: false`. [lith]
- A page too slow for its stream falls behind with no sign on the server:
  the browser holds the frames (Chromium hands the page 2 MiB per read,
  about 45 Game of Life frames applied in one task without a paint; Firefox
  holds them in its parent process and hands over one per task). Load
  `newest-frame.js` (`live.NewestFrame`, vendored from
  github.com/mbolli/datastar-attribute-newest-frame) after Datastar through
  the import map and put `data-newest-frame` on `<body>`: it applies each
  view's newest frame, waits after a batch half as long as the batch took,
  and after dropping over half the frames for 3 s dispatches
  `newest-frame-behind` on the stream's element, again every 3 s while
  behind. Listen for it on `<body>`, never on the stream's element (its
  `@post` would abort the stream's `@get`): the template posts it to
  `/pace`, which counts it (`live.Hub.Behind`) and paces the stream by the
  page's acknowledgements until the page keeps up (`live.Hub.Pace`). With `__reopen` the
  plugin also sets `$_slow` and opens the stream again, which sends
  `Save-Data: on` at the same URL, for a slow stream of four frames a
  second for good; not the default, since a page only somewhat slow gets
  fewer frames from it. A new URL would leave the old request open, its
  buffered frames still arriving.
  A trace at CPU 15x: every read exactly 2,097,152 bytes, 45 frames, one
  7.2 s task, two paints in 20 s. Without `no-store` on the stream,
  Chromium routes it through its HTTP cache, two or three reads land in one
  task and the freeze grows to 14.6 s: keep `no-store`. [lith]
- An action's response carries nothing the view needs: the outcome arrives on
  the tab's stream. Patching elements from an action fights the next frame,
  which overwrites them. A validation message is an outcome in the tab's
  row too. Signals in an action's answer only for a navigation (the `_goto`
  after creating a board). [hyperlith README, kanban]
- `datastar-fetch`'s `error` event carries the status (`argsRaw.status`),
  never the body: a message in a 4xx or 5xx body never reaches the person.
  Tell them on the stream (an outcome, or `live.Hub.Notify`), with the op
  id; the page's own words only for `retries-failed` and a stream that does
  not connect. [lith]

## Security headers

- Datastar compiles expressions with `Function()`: the CSP needs
  `'unsafe-eval'`. The violation is not a console error, so a page can look
  fine and do nothing.
- An inline `<script>` needs the request's CSP nonce. [starbase]

## Testing

- "The server sends the right bytes" and "the page works" are different
  claims. A Go test that reads the stream over HTTP proves the first only.
  Drive the page in a real browser for the second: starbase's Chrome tests,
  kanban's e2e harness.
