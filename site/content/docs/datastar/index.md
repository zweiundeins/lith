---
title: Datastar notes
headline: Datastar 1.0.4 notes: symptoms, causes and fixes
summary: The Datastar behaviours that cost the lith projects time, each with what you see, why it happens, and what to do. Written against Datastar 1.0.4, the version the template and this site vendor.
description: Datastar 1.0.4 behaviours that cost the lith projects time, in attributes, signals and morphs: what you see, why it happens, and what to do about it.
order: 4
---

The template and this site vendor Starbase's patched build of Datastar 1.0.4, with Rocket. Most of what follows is plain Datastar behaviour that surprised someone; a few notes are about how a lith page uses it.

## Attributes

### An attribute with a hyphen where a colon belongs does nothing

**Symptom.** A form submits the old-fashioned way, as a GET with its fields in the address, or an input binds to nothing. The console stays empty.

**Cause.** Datastar 1 reads attributes as `data-<plugin>:<key>__<modifier>`: `data-on:submit__prevent`, `data-bind:email`. A hyphenated form such as `data-on-submit` names a plugin that does not exist, and Datastar ignores the attribute without an error.

**Fix.** A colon between the plugin and its key, two underscores before each modifier. A template test that looks for the right attribute names catches the slip before a browser does.

### A camelCase key binds a second, empty signal

**Symptom.** `data-bind:fullFare` binds an input, but the code that reads `$fullFare` sees nothing typed.

**Cause.** HTML lowercases attribute names, so the browser hands Datastar `data-bind:fullfare`, and the input binds a signal named `fullfare`.

**Fix.** Write keys in kebab-case, `data-bind:full-fare`, which Datastar turns into `fullFare`.

### data-on:load on a div never runs

**Symptom.** A stream opened with `data-on:load` never opens; other work started that way never starts.

**Cause.** `data-on:load` listens for the element's own `load` event, and only elements that load something fire it: an image, an iframe, a script, a stylesheet link. A div never does, and the page's own `load` event goes to `window`.

**Fix.** Open streams and start work with `data-init`, which runs when Datastar sets the element up.

### A data-init delay outlives its element

**Symptom.** Something happens after its element left the page: a message hides a later one early, a reveal ends too soon.

**Cause.** `data-init__delay` starts a timer that nothing clears: it fires even after its element is gone.

**Fix.** A timer that must end with its element is `data-on-interval__duration`, which Datastar clears when the element goes; its expression acts once and then does nothing.

### A regex in filterSignals needs its slashes

**Symptom.** An action stops working altogether, not only its filter.

**Cause.** `filterSignals` takes regular expressions as JavaScript literals. Without the slashes, the pattern is a bare identifier or a syntax error, and the whole expression fails to compile.

**Fix.** `{filterSignals: {include: /^_form\./}}`, slashes included.

### An apostrophe in copy breaks an expression

**Symptom.** One translation, or one message, makes an element's expression throw, and the element does nothing.

**Cause.** Copy that ends up inside an expression sits in a single-quoted JavaScript string, and a straight apostrophe in the copy, as in "don't", closes the string.

**Fix.** Keep copy out of expressions: put it in the element's text and show or hide it, or in a signal the server sets. Where it must sit in an expression, quote it from Go with a JSON encoder, or write the typographic apostrophe.

## Signals

### Signals that start with an underscore stay in the browser

**Symptom.** Every action sends the page's whole state: a long text being typed, a password, a list the page only displays.

**Cause.** An action sends every signal of the page, except those whose name starts with `_`.

**Fix.** Name view-only and ephemeral signals with a leading `_`: `$_pending`, `$_size`, the tab's id, what someone is typing. An action then names what it sends in its payload, as the board's actions do. A password lives in a `_` signal too, and only the login's action sends it.

### Assigning an object merges, and null deletes

**Symptom.** A signal holds keys that the code no longer sets, or an effect stops updating after the signal was cleared.

**Cause.** Assigning an object to a signal merges it into the signal, so keys are never removed, and `null` deletes the signal itself, which leaves every effect that read it stale.

**Fix.** Clear with `false` or `''`, never `null`. Keep maps whose keys come and go in plain variables, and expose what the template renders.

### data-bind on a custom element needs its signal first

**Symptom.** A web component's value never reaches its signal.

**Cause.** `data-bind` on a custom element only writes to a signal that exists. Datastar binds before the component upgrades, and finds nothing to write to.

**Fix.** Declare the signal with `data-signals` first, on the element or above it.

## Morphs

### A morph resets what someone is typing

**Symptom.** A field loses its text, or a `<details>` closes, whenever a frame arrives.

**Cause.** A frame renders the region whole, and the morph makes the page match it: the server's HTML has the field empty and the `<details>` closed, and the morph resets attributes that the server did not render, including ones a signal set.

**Fix.** Keep fields out of regions the stream morphs, with their values in signals, as the board's add form is. A field that must sit in a morphed region gets `data-ignore-morph`. A `<details>` gets `data-preserve-attr="open"`, and an element whose attributes come from signals gets `data-preserve-attr` with their names.

### Keyed elements with Rocket components crash the morph

**Symptom.** A frame throws `Maximum call stack size exceeded`, or a `moveBefore` error, and the page stops updating.

**Cause.** Datastar 1.0.4's morph is not re-entrant, and a Rocket component renders while it is being connected. Elements with ids that hold Rocket components, and repeat or move, set off a morph inside a morph.

**Fix.** Use Starbase's patched build, which the template and this site vendor, until a Datastar release carries the fix.

### Expressions on many repeated elements cost every frame

**Symptom.** Frames of a large list grow, and the page slows down as each frame lands.

**Cause.** Each expression is markup in every frame, and Datastar compiles it when the element arrives: 200 cards with three expressions each are 600 expressions per frame.

**Fix.** Put the behaviour on the container and read the item from data attributes, as the board does with one click handler on `#board` ([Pages and Datastar](/docs/guides/pages#regions-rendered-whole)).

### A view transition from the stream takes the page's clicks

**Symptom.** While a frame's view transition plays, buttons and links on the page do nothing.

**Cause.** A patch with `useViewTransition` (`sse.Patch{ViewTransition: true}`) runs a view transition of the whole document. It draws snapshots over the page, and events cannot target those, so until it ends Chromium gives every press to `<html>`. A view that animates a few times a second leaves the page dead most of the time.

**Fix.** Scope the transition with `ViewTransitionSelector` to the element that holds what moves, usually the one the frame patches. Datastar then calls that element's `startViewTransition`, and the rest of the page keeps taking clicks. Inside the scope nothing takes a press until it ends: the press goes to what lies beneath the scope, so a drag that starts there finds its target by position, as this site's shared board does. A browser without scoped transitions morphs at once, without the animation.

## Requests and streams

### The default retry leaves a page stale after a deploy

**Symptom.** After a deploy, open pages stop updating. Nothing shows it; a reload fixes it.

**Cause.** Datastar retries a request by its `retry` option. `'auto'`, the default, retries network errors only: it gives up at the proxy's 502 while no process listens, and never reopens a stream the server ended cleanly.

**Fix.** Open streams with `retry: 'error', retryMaxCount: Infinity`. `'error'` also retries 4xx and 5xx answers, so it carries the request through a restart. `'always'` reopens a stream even when the server ended it on purpose, and keeps a full server's 503s coming. An answer of 204 stops every mode, which is how a server refuses a stream for good.

### The server tells the page when to reconnect

**Symptom.** With `retry: 'error'`, a stream that the server ended cleanly still stays closed.

**Cause.** A stream that ends with a complete response is not an error, so no retry mode but `'always'` opens it again.

**Fix.** Open the stream with `data-init` on an element with an id, and pass that id as `live.Spec.Reconnect`. When the hub stops for a deploy, each stream's last event is `sse.Reconnect`, which replaces the element with a copy of itself; the copy's `data-init` runs again with its own options, and `'error'` carries the request through the restart. A stream the server ends for another reason, such as a newer stream of the same tab, stays closed.

### The element that reconnects does nothing else

**Symptom.** After a deploy, a page has a new tab id, or a signal it set up has its first value again.

**Cause.** Every deploy runs the `data-init` of the element that `Spec.Reconnect` names again.

**Fix.** That element opens the stream and nothing else. The tab's id and the page's signals live on another element, rendered once with the document.

### A watchdog never sees the server's heartbeat

**Symptom.** A connection that went quiet without closing, after a laptop's sleep or a change of network, leaves the page frozen, and a watchdog written to catch it never fires.

**Cause.** The hub keeps a silent stream open with an SSE comment every 20 s, and Datastar never passes comments to the page.

**Fix.** Set `live.Options.HeartbeatSignal`: the heartbeat then patches that signal every 20 s, frames or not, and the page's watchdog opens the stream again when it stops changing ([Views and the hub](/docs/guides/views#the-heartbeat-and-the-pages-watchdog)).

### A new request cancels the one in flight

**Symptom.** Of a quick series of actions, a drag that paints pixels or several moves, some never reach the server.

**Cause.** By default, Datastar cancels the request an element has in flight to a URL when the element sends another one to the same URL.

**Fix.** Actions that must all arrive set `requestCancellation: 'disabled'`. Give each an op id, so a retried one runs once ([Commands](/docs/guides/commands#op-ids-and-retries)).

### Signals arrive in the address or in the body, by verb

**Symptom.** A handler that reads the query string finds no signals in a `@post` or a `@query`, and one that reads the body finds none in a `@get`.

**Cause.** Datastar sends the signals, or the `payload` an action names, as the `datastar` query parameter for `@get` and `@delete`, and as a JSON body for every other verb, `@query` included.

**Fix.** Read them with `sse.ReadSignals(w, r, &v)`, which looks where the method puts them, and call it before `sse.Open`, whose headers discard the body (it then returns `sse.ErrBodyConsumed`). Pick the verb by what the request does: `@get` for streams and for reads whose signals are all `_`, `@query` for reads that send real signals (a search, a filter), `@post` for commands ([Pages](/docs/guides/pages#verbs-and-signals)).

### A stream opened with @query stays open in a hidden tab

**Symptom.** A page whose stream opens with `@query` keeps it, and its encoder on the server, while the tab sits in the background. A page that opens its stream with `@get` drops it there and opens it again when the tab shows.

**Cause.** Datastar's `openWhenHidden` defaults to false for `@get` only, which closes the request while the page is hidden. Every other verb defaults to true.

**Fix.** A stream that opens with `@query` sets `openWhenHidden: false`, unless it should run in the background.

### A page that falls behind its stream stops painting

**Symptom.** On a slow phone, a page with a busy stream shows frames from further and further back, a minute behind after a few minutes, while the server reports nothing wrong. In Chromium the page also stops painting for seconds at a time and ignores input; in Firefox it keeps painting, just as far behind.

**Cause.** Datastar applies every event of the stream, in order, as it reads them. When the main thread is slower than the stream, the frames queue in the browser, and the server's writes never wait: the buffers on the way hold minutes of a page's frames. Once the page is behind, every read in Chromium returns 2 MiB, the size of the pipe between its network process and the page: about 45 frames of the Game of Life, which Datastar applies in one task, 7 s of morphing at 150 ms a frame, with no paint between. A performance trace of the Game of Life with Chromium's CPU slowed 15 times shows it (2026-10-16, `bench/drift/RESULTS.md`): once the page was behind, every read was exactly 2,097,152 bytes, 45 frames, applied in one task of 7.2 s, and the page painted twice in 20 s. The first reads after the page falls behind are smaller, 370 KB to 1.1 MB. lith sends every stream with `Cache-Control: no-store`. Without it, Chromium passes the stream through its HTTP cache, and two or three reads can land in one task: the longest grew to 14.6 s, with 90 or 91 frames, against 7.8 s with `no-store`, and the page fell just as far behind. Keep `no-store`. Firefox keeps the frames in its parent process and hands the page one at a time, each in a task of its own. No browser API tells a page that it is behind its stream: Long Animation Frames report a frame after it painted, and a stream's response never ends, so Resource Timing never records it.

**Fix.** Load lith's `newest-frame.js` (`live.NewestFrame`) as a module after Datastar, through the import map that maps `'datastar'`, and put `data-newest-frame` on `<body>`. lith vendors the plugin's build from [datastar-attribute-newest-frame](https://github.com/mbolli/datastar-attribute-newest-frame), which has its source, its tests and a live demo. It holds the stream's patch events and applies only the newest frame of each view among those that arrived together, and after a batch it waits half as long as that batch took to apply, which gives Firefox the time to hand over what it holds. A page that keeps up applies every frame. When the page drops more than half of its frames for 3 s, the plugin dispatches `newest-frame-behind` on the stream's element, and again every 3 s while the page stays behind. The template's `<body>` posts it to `/pace` (`data-on:newest-frame-behind`): the server counts it with `live.Hub.Behind`, which tells whether real users fall behind ([Streams: counting the pages that fall behind](/docs/guides/streams#counting-the-pages-that-fall-behind)), and paces the stream by the page's acknowledgements (`live.Hub.Pace`), so that a slow page gets only the frames it applies, until it keeps up again ([Streams: pacing a stream by the page](/docs/guides/streams#pacing-a-stream-by-the-page)). Listen on `<body>`, where the event bubbles to, not on the stream's element: a `@post` from there would abort the stream's own `@get`. With `__reopen` the plugin also sets `$_slow` and opens the stream again; the stream's `@get` sends `Save-Data: on` while `$_slow` is true (`headers: $_slow ? {'Save-Data': 'on'} : {}`), and the server's `edge.SlowLink` gives it a slow stream of four frames a second, for good. That is not the default: a page only somewhat slow gets fewer frames from it than without it. Keep the stream's URL the same: Datastar cancels the request in flight only when the new one goes to the same URL, and a stream opened at another URL leaves the old one open, its buffered frames still arriving ([Streams: slow links](/docs/guides/streams#slow-links)).

### Elements patched from an action's answer get overwritten

**Symptom.** An action patches an element in its answer, and the next frame puts the old content back.

**Cause.** The next frame renders the same region from the database, and the frame wins.

**Fix.** An action answers 204, and the outcome arrives on the stream with the next frame, rendered from the state it changed. Signals in an answer are fine for what concerns that tab alone, such as a validation message or a navigation.

## Security headers

### Without 'unsafe-eval', the page does nothing

**Symptom.** The page renders, and nothing on it reacts, and no error says why.

**Cause.** Datastar compiles its expressions with `Function()`, which a Content Security Policy blocks unless `script-src` has `'unsafe-eval'`.

**Fix.** Keep `'unsafe-eval'` in `script-src`. `edge.DefaultCSP()` has it.

### An inline script needs the request's nonce

**Symptom.** An import map or another inline `<script>` does not run.

**Cause.** The policy allows scripts from the site's own files and, inline, only with the request's nonce.

**Fix.** Give the tag `nonce` from `edge.Nonce(ctx)`, which `edge.SecurityHeaders` sets for every request ([The request edge](/docs/guides/edge#security-headers-and-the-nonce)).

## Testing

### A test of the stream does not test the page

**Symptom.** Every Go test passes, and the page in a browser does nothing.

**Cause.** "The server sends the right bytes" and "the page works" are different claims. A Go test that reads the stream over HTTP proves the first only; attribute syntax, the CSP and the morph are the browser's.

**Fix.** Drive the page in a real browser and check its console: this site's tests run Chromium on every page, and among the projects starbase has its Chrome tests and kanban an end-to-end harness.
