---
title: Pages and Datastar
nav: Pages
headline: Pages with Datastar: regions, tab state, op ids
summary: The document a GET renders, regions morphed whole, a tab's state in SQLite, an action that stays pending until its op id comes back, and a page that reconnects by itself after a deploy.
description: The document a GET renders, regions morphed whole, tab state in SQLite, actions pending until their op id returns, and reconnecting after a deploy.
order: 6
---

A lith page is a document that a GET renders from one read transaction, and a stream that patches it from then on. [Datastar](https://data-star.dev) runs in the browser: it sends the actions, keeps the page's signals, and morphs the HTML that arrives on the stream into the page by the elements' ids. The [Datastar notes](/docs/datastar) list what goes wrong with it, and why.

## The document

The board's document holds the page's signals, the element that opens the stream, the element that keeps the page's ops, a place for a notice after a deploy, and `#content`: the add form and the board.

```templ source=site/internal/docs/example/web/board.templ
<main data-signals={ signals(p.Tab, p.Build, p.Prefs) } data-class:compact="$_compact">
	<div id={ StreamID } hidden data-init={ "$_beat = Date.now(); " + openStream(p.Board.ID) } data-on:datastar-fetch={ retrying } data-on:visibilitychange__window={ hidden } data-effect={ beat } data-on-interval__duration.10s={ watchdog(p.Board.ID) }></div>
	<div id={ OpsID } hidden data-on:datastar-fetch={ fetched } data-on-signal-patch={ patched } data-on-signal-patch-filter={ opsPatches }></div>
	<p id="update" role="status"></p>
	@Content(p.Board, p.Lanes)
</main>
```

```templ source=site/internal/docs/example/web/board.templ
templ Content(b board.Board, lanes [][]byte) {
	<div id="content">
		<h1>{ b.Name }</h1>
		@Presence(0)
		<button type="button" data-on:click={ compact } data-text="$_compact ? 'Show details' : 'Compact'">Compact</button>
		<form data-on:input={ typed } data-on:submit__prevent={ add(firstLane(b)) }>
			<input data-bind="_title" maxlength={ strconv.Itoa(board.MaxTitle) } required aria-label="New card" data-attr:readonly={ pending("add") } data-preserve-attr="readonly"/>
			<button type="submit" data-attr:aria-busy={ busy("add") } data-preserve-attr="aria-busy">Add</button>
		</form>
		<p class="problem" role="alert" data-show="$_problem" data-text="$_problem"></p>
		@Outcome(board.TabState{})
		@Board(lanes)
		@NoPanel()
		<style data-text={ waitingCards }></style>
	</div>
}
```

The expressions live in Go, in `expr.go`, beside the templates that use them. Every signal starts with `_`: such signals stay in the browser, and an action sends only the payload it names. The tab's id, `$_tab`, is new for every document, so two tabs of one browser are two tabs to the server; the server never stores it until the tab acts.

```go source=site/internal/docs/example/web/expr.go
func signals(tab, build string, p board.Prefs) string {
	return fmt.Sprintf(`{_tab: %q, _title: '', _ops: {add: {op: '', ack: '', refused: false, inflight: 0, recheck: false}}, _synced: false, _problem: '', _compact: %t, _ping: 0, _beat: 0, _build: %q, _dirty: false}`, tab, p.Compact, build)
}
```

The document answers with `Cache-Control: no-store`: the tab id is this document's alone, and a cached copy would give two tabs one id.

## Regions rendered whole

A region of the page is rendered whole, from the database, every time it changes: the board renders all its lanes, a lane all its cards. Datastar's morph compares the new HTML with the page, element by element, matched by their ids, and changes only what differs: an element that did not change keeps its DOM node and its focus. So the server needs no diff, and the page needs no reducer that applies one.

- Give every element that repeats or moves an id: `lane-2`, `card-17`. The morph matches elements by their ids, and without one it can take one card for another.
- The same template renders the document and the stream's frames: `Board` and `Lane` serve the GET and the view alike.
- A field someone types in does not belong in a region the stream morphs: the next frame would reset it. The add form sits outside the board, and its value lives in a signal (`data-bind="_title"`). An input that must sit in a morphed region gets `data-ignore-morph`.
- Behaviour for many repeated elements goes on their container: the board handles every card's click in one expression on `#board`, reading the card from data attributes, so a frame carries no expression per card.

```templ source=site/internal/docs/example/web/board.templ
templ Board(lanes [][]byte) {
	<div id="board" data-on:click={ onBoard }>
		for _, l := range lanes {
			@templ.Raw(string(l))
		}
	</div>
}
```

## Tab state in SQLite

State that belongs to one tab, such as the outcome of its last action or the panel it has open, is a row keyed by the session and the tab id, written by a command:

```go source=site/internal/docs/example/board/tabs.go
type TabState struct {
	Ops     map[string]Ack `json:"ops,omitempty"`
	Refusal string         `json:"refusal,omitempty"`
	Panel   int64          `json:"panel,omitempty"`
}
```

`Ops` holds the newest op of each op key, the add form's and each card's, and whether it was refused ([Several pending ops](#several-pending-ops-on-one-page)).

It is a row, not a map in the server's memory, so it survives a deploy: the page reconnects to the next process, and the stream's handler finds the open panel in the row ([Views and the hub](/docs/guides/views#adding-a-view-to-an-open-stream)). A map of tabs in memory is empty after every restart. The sweep deletes rows of tabs that stayed quiet for a week.

A preference that should outlive the page, and apply in every tab, is a row keyed by the session: the board's compact mode. The GET renders it into the document, so the page never flashes the other setting, and the session's view patches it into every open tab when it changes.

What only the browser needs, what someone is typing, an open menu, stays in `_` signals and never reaches the server.

## Op ids and the pending state

A click shows the user's own input at once and marks it pending. The outcome, a new card or a refusal, comes with the next frame, on the stream. The page never shows an outcome before the server has decided it, so there is nothing to roll back.

Every op has a key: what the person acted on, such as the add form (`add`) or a card (`card-17`). `$_ops` holds one entry per key: the pending op id, the newest op the tab's row acks and whether it was refused, the key's requests in flight, and whether it must look again when they end.

1. The action makes an op id and posts its payload, unless an op of its key is pending. The tab, the op key and the op id go in the `Tab-Id`, `Op-Key` and `Op-Id` headers:

```go source=site/internal/docs/example/web/expr.go
func act(key, path, payload string) string {
	return `((k, p) => {
	const e = $_ops[k];
	if (e && e.op && e.ack !== e.op) return;
	$_problem = '';
	const op = Date.now().toString(36) + Math.random().toString(36).slice(2, 7);
	$_ops[k] = {op: op, ack: e?.ack ?? '', refused: false, inflight: e?.inflight ?? 0, recheck: false};
	const c = document.createElement('i');
	c.dataset.opKey = k;
	c.setAttribute('data-init', "@post('` + path + `', {headers: " + JSON.stringify({'Tab-Id': $_tab, 'Op-Key': k, 'Op-Id': op}) + ", payload: " + JSON.stringify(p) + ", requestCancellation: 'disabled'})");
	document.getElementById('` + OpsID + `').append(c);
})(` + key + `, {` + payload + `})`
}
```

Headers, so that the handler knows the tab and the op before it reads the body: an action over the rate limit gets its 429 and the tab its notice without the body being read or decoded ([Commands](/docs/guides/commands#outcomes-notices-and-the-pages-own-words)). Headers are safe here and not on the stream: Datastar builds a request's headers once, when its expression runs, and repeats them on a retry. An op's element runs its `data-init` once, with that op's tab and id, so a retry should repeat them; the stream's element reopens with values that have changed since ([Refreshing a page after a deploy](#refreshing-a-page-after-a-deploy)).

The request does not go out from the form or the button. It goes out from an element made for it, inside `#ops`, which names the op's key in `data-op-key`. `#ops` is hidden and sits outside every region a frame morphs, so no frame removes it. The form may sit in a dialog that closes, a menu that folds, a card that the next frame moves or removes: the request's events still reach `#ops`, which counts them and ends the op. A listener on the form would miss the end of a request whose form went away, keep counting it as in flight, and wait for ever.

`requestCancellation: 'disabled'`: Datastar 1.0.4 cancels a request when another one goes out with the same method to the same URL, from anywhere on the page. Two cards moved at once both post to `/cmd/move`, and the second would abort the first.

2. While an op of the key waits for its ack, what it came from is pending: the input is read-only and the button has `aria-busy`, for screen readers and for the stylesheet. A card's pending move fades the card, from the same signals:

```go source=site/internal/docs/example/web/expr.go
func pending(key string) string {
	e := `$_ops['` + key + `']`
	return `!!` + e + `?.op && ` + e + `.ack !== ` + e + `.op`
}

// busy is pending for aria-busy: data-attr writes true as an empty value,
// which aria-busy reads as false.
func busy(key string) string { return `(` + pending(key) + `) ? 'true' : false` }
```

3. The handler wraps the command in `writer.Once` with the op id and answers 204 ([Commands](/docs/guides/commands#op-ids-and-retries)). The command records the op under its key, and any refusal, in the tab's row. When no command decides, the writer failed or a rate limit turned the action away, the handler sends the tab a notice with the op id instead ([Commands](/docs/guides/commands#outcomes-notices-and-the-pages-own-words)).

4. The tab's view renders the row and patches each key's ack into `$_ops`, and a notice patches its key's ack the same way. `patched`, on `#ops`, ends an op when its ack arrives, and empties the title when a card was added:

```go source=site/internal/docs/example/web/expr.go
const patched = `(() => {
	for (const k in patch._ops || {}) {
		const e = $_ops[k];
		if (!!e.op && e.ack === e.op) {
			e.op = '';
			if (k === 'add' && !e.refused) $_title = '';
		}
	}
	if (patch._synced) for (const k in $_ops) {
		const e = $_ops[k];
		if (!e.op || e.ack === e.op) continue;
		if (e.inflight > 0) e.recheck = true;
		else { e.op = ''; $_problem = 'That did not go through. Try again.' }
	}
})()`
```

`data-on-signal-patch` runs it when signals change, and `data-on-signal-patch-filter={ opsPatches }`, `{include: /^(_ops\.[^.]+\.ack|_synced)$/}`, keeps it to the acks and `$_synced`, so a click alone never runs it. An effect would also run when the op starts, and end it at once. Datastar splits a `data-on-signal-patch` expression at its semicolons, so it is one function call.

5. `fetched`, on `#ops` too, follows every op's requests by the key their element names. It counts them in flight, per key, and ends an op in two cases the stream cannot tell:

```go source=site/internal/docs/example/web/expr.go
const fetched = `const c = evt.detail.el, k = c && c.dataset && c.dataset.opKey;
if (!k) return;
const e = $_ops[k], t = evt.detail.type, waiting = !!e.op && e.ack !== e.op;
if (t === 'started') e.inflight = e.inflight + 1;
else if (t === 'error') {
	const s = +evt.detail.argsRaw.status;
	if (waiting && s !== 409 && s !== 429 && s < 500) { e.op = ''; $_problem = 'That did not work. Try again.' }
} else if (t === 'retries-failed') {
	if (waiting) { e.op = ''; $_problem = 'The connection is down. Try again once it is back.' }
} else if (t === 'finished') {
	e.inflight = Math.max(0, e.inflight - 1);
	c.remove();
	if (e.inflight === 0 && e.recheck) { e.recheck = false; if (!!e.op && e.ack !== e.op) ` + reopen + ` }
}`
```

- A request that never got through: Datastar's `retries-failed`, after its retries. Only the browser knows it, so the page says so in its own words.
- An answer with a status that comes without a notice. The handlers send a notice with every 429 and every 5xx, or answer 503 as the stream goes away, so those leave the op to the stream; so does a 409, which the template answers for a tab whose stream is not open, and whose reconnect then decides. Any other 4xx is a request no page of ours sends, which gets a bare 400, and no notice will end its op: the page ends it at once, "That did not work." The status picks the case, not a clock. So every check a person can fail must be a refusal the command records ([Refusals](/docs/guides/commands#refusals)): a person who could reach a 400 would get the page's vague words instead of the reason.

Datastar's fetch events reach every listener on the page, so `fetched` takes only the requests whose element names a key. Never a response's body: a non-200 body is ignored, and a person reads nothing from a status.

6. A stream that connects again without the op's ack ends it too, once no request of its key is in flight: the second half of `patched`. A notice goes only to the stream open when it is sent: one that a reconnect cut off, or one sent while the tab had no stream, never arrives. Both happen around a reconnect, and the reconnected stream's first frame of the tab's view holds the tab's row. When that row does not ack a pending op, the page ends it in its own words and keeps what was typed.

The page tells the first frame after a (re)connect by `$_synced`. Opening the stream sets it false (`openStream`), and so do Datastar's `retrying` on the stream's element and a tab that hides (`hidden`, on `visibilitychange`): Datastar closes the stream of a hidden tab and opens it again when the tab shows, without `data-init` and without `retrying`. The tab's view patches `_synced: true` with every frame, so the signal changes only with the first frame after a (re)connect. Each of the three must set it false: `data-on-signal-patch` fires only when a value changes, so a reopen that left `$_synced` true would bring a `true` that changes nothing, and `patched` would never run for it.

While a request of the key is open, nothing is decided: its command may still wait in the writer, or the click came before the stream's first frame. Ending the op then would tell the person "did not go through" about a command that commits a moment later, and their retry, with a new op id, would add a second card. So `patched` only notes that the key must look again (`recheck`), and when the key's last request ends, `fetched` opens the stream once more (`reopen`, as `sse.Reconnect` does). By then the answer is in: a 204 means the command committed, so the fresh frame of the tab's row holds the ack; a failure status means a notice was sent, perhaps to a stream that was gone, and the fresh frame without the ack ends the op in the page's words.

The pending state ends on a signal, never on a clock: the ack, `retries-failed`, a status without a notice, or a reconnect without the ack. A command may rightly take minutes, and no fixed time tells a slow answer from a lost one.

During a blue/green deploy, a command can commit in the old process while it drains, after the page's stream moved to the new one. The new hub never hears of that commit, so no frame brings its ack; the request's answer, a 204, ends it on the page all the same, since `fetched` opens the stream once more. The stream that opens replaces the tab's stream on the new process, and renders the views only that tab watches afresh, so the tab's row comes as the database now holds it. A view that other tabs watch too, the board, comes from the frame the new hub keeps, and shows the old process's commit only when the next commit touches its topics: the op ends, but the card appears later. Call `hub.MarkAll()` on the new process once the old one has stopped, or after the drain window: every view renders again from the database. A deploy that stops the old process before the new one starts has no such gap: every commit is in the database before the new stream's first frame. The template's slots and the site run one process at a time.

### A form that posts an op

A form whose action sends it with `contentType: 'form'` needs `novalidate`, with the expression checking the fields before it acts. Datastar 1.0.4 runs the browser's `checkValidity` on such a request after it has sent `started`, and when the check fails it ends the request without `finished`: the key would stay in flight for ever. The board's form sends a JSON payload instead, so the browser checks `required` and `maxlength` before the submit event, and no request starts.

## Long operations

The pending state means "the server has taken my request", not "the work is done". A command that starts long work, an import, an export, a report, first records its acceptance in the tab's state, "Import started", which acks the op at once. The work then reports its progress through state a view renders, so it survives a reload, and its end is another outcome in the same place. With the acceptance recorded first, a reconnect never meets an op the server has taken without its ack, and while the request waits in the writer's queue, the page waits for its answer before it decides.

## Several pending ops on one page

The board keeps an op per key: the add form's, and each card's move. Each key is pending on its own, so a person can move two cards and add a third while all three wait. The entry of each key holds its own requests in flight and its own `recheck`: a request that hangs, a move the writer holds or a proxy keeps, holds back only its own key, and a lost add on the same page still ends on the next reconnect. One count for the page would keep every op pending as long as any request is open.

The tab's row keeps the newest op of each key and whether it was refused (`TabState.Ops`), and forgets the key with the oldest op beyond 16. The tab's view patches all of them, and a notice takes the key of its op (`notice:add`, `notice:card-17`), so a notice for one key never replaces another's. The move's key comes from the card's button, in the one handler on `#board`; the fade of a card whose move waits comes from `$_ops` in a stylesheet the page writes itself (`waitingCards`), outside the regions the stream morphs.

## Where the op's ending lives

`fetched` and `patched` sit on `#ops`, which no frame removes, and so must any toast or message the page shows when an op ends. The place the person acted may be gone by then: the dialog closed, the menu folded, the card moved to another lane or out of the list. That is the case the feedback rules send to a toast ([Which feedback to give](#which-feedback-to-give), rule 4), and a listener on the form or the card would not be there to show it.

## Uploads

A file goes over plain `fetch`, not a Datastar action, and the rules stay. The tab and the op travel in the query string or in headers, since a body over `http.MaxBytesReader`'s limit cannot be parsed for them. The handler checks the file, too large or of the wrong type, and records the outcome with a command, as `Act` records a refusal, before it answers: the tab's view brings the ack and the reason.

A plain `fetch` sends no `datastar-fetch` events, so the page does what `fetched` does by hand, in an expression on the file input's `change`. It checks the size first, sets the key's entry as `act` does and raises its in-flight count before the request; in `finally` it lowers the count, and when the count is 0 and `recheck` is set, it clears it and opens the stream once more if the op still waits:

```js
(async (file, op) => {
  const e = $_ops.upload
  if (file.size > 10 << 20) { $_problem = 'A file has 10 MB at most.'; return }
  e.op = op; e.refused = false; e.recheck = false; e.inflight = e.inflight + 1
  try {
    const r = await fetch('/upload?tab=' + $_tab + '&op=' + op, {method: 'POST', body: file})
    if (r.status !== 409 && r.status !== 429 && r.status >= 400 && r.status < 500 && e.op === op) { e.op = ''; $_problem = 'That did not work. Try again.' }
  } catch {
    e.recheck = true // no answer: the stream decides
  } finally {
    e.inflight = Math.max(0, e.inflight - 1)
    if (e.inflight === 0 && e.recheck) {
      e.recheck = false
      if (e.op === op && e.ack !== op) (s => s.replaceWith(s.cloneNode(true)))(document.getElementById('stream'))
    }
  }
})(el.files[0], Date.now().toString(36) + Math.random().toString(36).slice(2, 7))
```

The size check in the page saves the person the wait; the handler still checks, and records its refusal. `edge.BodyDeadline` gives a body 10 s, too little for 10 MB on a slow link, so the upload's handler sets a deadline of its own before it reads, such as `http.NewResponseController(w).SetReadDeadline(time.Now().Add(2 * time.Minute))`, and Caddy's `read_body` must allow as long ([Request bodies](/docs/guides/edge#request-bodies)). A server that answers "too large" before it has read the body makes Go close the connection after about 256 KB it did not read, and Chrome then often rejects the `fetch` with `ERR_CONNECTION_RESET`, though the outcome was recorded. So a rejected `fetch` is no proof that the connection is down: the page treats it as an op whose answer was lost, and the stream decides, with the ack, or with the reopen that finds none. "The connection is down" is for a stream that is down.

## A refusal next to where the user acted

The tab's view renders the refusal into `#outcome`, beside the form, in this tab only:

```templ source=site/internal/docs/example/web/board.templ
// Outcome is the tab's own region: why its last action changed nothing.
templ Outcome(s board.TabState) {
	<p id="outcome" role="status">{ s.Refusal }</p>
}
```

Other tabs see the board change, or not change, and nothing else.

## Which feedback to give

A page has several ways to tell the person something:

- **The re-render**: the changed data itself, morphed into the page from the stream.
- **An inline error**: the form's error slot, or the field itself.
- **Standing state**: a banner, a badge or a status rendered from data, shown while the condition lasts.
- **A toast**: a card that does not block, goes away by itself and has one action at most.
- **Progress**: what work outside a request shows, an upload, an import, an export.
- **A dialog**: a question the person must answer, to confirm, to type something, to choose.

Apply the rules in order; the first that fits decides.

| # | When | Give |
| --- | --- | --- |
| 1 | The change shows where the person acted | nothing more: the re-render confirms it, and a toast would repeat it |
| 2 | The person must fix something: a refusal, an input check | an inline error where they acted; never a toast, never a dialog |
| 3 | Something holds until the person acts: a limit reached, a backup that is unhealthy | standing state, rendered from data while it lasts; never a toast, which goes away |
| 4 | It happened where the person cannot see it: the place they acted is gone, the effect lands on another page, the item left the list, a long command was accepted and its progress follows from state | a toast |
| 5 | An offer or a celebration | a toast with its one action; missing it must do no harm |
| 6 | A notice, which the person did not cause: the writer failed, a rate limit, a restart | a toast at warning or error level, in the page's words, never a response's body |
| 7 | Only the browser knows: no connection, a request that did not go through | a toast in the page's own words |
| 8 | A question | a dialog, only to ask, never to report |

The board follows them: a card that appears in its lane is rule 1, and nothing else is said; a refusal in `#outcome`, beside the form, is rule 2; "The connection is down" in `$_problem` is rule 7; a rate limit's or a failed write's notice is rule 6. The board has no toasts, so an inline line stands in for one: its notices take the outcome's place, and the page's own words go to `$_problem`, a line under the form with `role="alert"`, which screen readers announce as they would a toast. A page that has toasts shows both as toasts.

How each reaches the page. Outcomes, rules 2, 4 and 5 after a command, are written to the tab's row by the command, and the tab's view renders them on the stream ([Commands](/docs/guides/commands#outcomes-notices-and-the-pages-own-words)). Notices, rule 6, are pushed on the open stream with `hub.Notify`, best effort. What only the browser knows, rule 7, the page says by itself. A status code never carries text for a person.

A toast for an outcome shows once per op. The tab's view renders the outcome's text with the op's ack, and the page shows the toast where it ends the pending state, in `patched` on `#ops`, which happens once, when it first sees that op's ack while the op is pending. It lives on `#ops` for the reason rule 4 gives: when the toast is needed, the place the person acted is often gone ([Where the op's ending lives](#where-the-ops-ending-lives)). A stream that reconnects renders the row again with the same ack, but the page has settled that op already, so no toast comes twice. After a reload, no op is pending: the row's outcome renders as state, in its region, and not as a toast.

## Reconnecting after a deploy

The stream opens from an element with an id, with Datastar's `retry: 'error'`:

```go source=site/internal/docs/example/web/expr.go
func openStream(b int64) string {
	return "($_synced = false, @get('/boards/" + strconv.FormatInt(b, 10) + "/stream?tab=' + $_tab, {headers: {'Page-Path': location.pathname + location.search}, filterSignals: {include: /^_(build|dirty)$/, exclude: /(?!)/}, retry: 'error', retryMaxCount: Infinity, retryMaxWait: 10000}))"
}
```

The stream's `Spec.Reconnect` names that element. When the server stops for a deploy, the hub ends every stream, and each one's last event, `sse.Reconnect`, replaces the element with a copy of itself: the copy's `data-init` runs and opens the stream again. During the restart the old process answers 503 while it stops, and the proxy 502 while no process listens; `'error'` retries both, and `retryMaxCount: Infinity` keeps trying until the new process answers. The new stream sends every view's current frame, so the page catches up with what changed meanwhile.

`'error'` retries a network error and every answer from 400 to 599, so a stream that gets 400, 401 or 429 asks again without end, each time with `retrying` and `error` events; a 204 or a 3xx stops it. So the stream's handler keeps 4xx and 5xx for what the next attempt may get past (the server stopping, a full hub), and answers 204 to end the retries for good. A stream whose page must tell the person something, a session that ended, opens all the same and renders it from a view, a sign-in link in the tab's region, rather than answer 401, which the page would never show.

- Datastar's default, `retry: 'auto'`, retries network errors only. It gives up at the proxy's 502, and never reopens a stream the server ended cleanly, so a deploy leaves the page stale without a sign.
- `retry: 'always'` reopens every stream that ends, including one the server ended on purpose: a stream that a newer stream of the same tab replaced, say.
- The element does nothing but open the stream, and mark the page out of sync until the stream's tab view speaks (`$_synced`): every deploy runs its `data-init` again, so anything else made there would be made again.

## Refreshing a page after a deploy

A stream that reconnects after a deploy renders its views afresh, but the rest of the page is still the HTML the old process rendered: a heading, a form's labels, a list that no view renders. A reader of a blog keeps the old list of posts until they reload. `live.Spec.Refresh` brings such a page up to date as its stream opens.

**What the page reports.** The page starts with the build that rendered it in `$_build`, from `hub.Build()`. That is `live.Options.Build`, by default the binary's VCS revision, with `+dirty` for a build from a modified tree; a binary built without VCS information has none, and refresh stays off until the app sets one, such as the version it links in (the template sets its `-X main.version`). `$_dirty` turns true on input in the form and false when the form is sent. The stream's request sends both as signals, in its `datastar` query parameter, and the page's own path, `location.pathname + location.search`, which never changes for a page, in a header, as `openStream` above shows. Datastar leaves signals that start with `_` out of a request unless `filterSignals` names them, and an `include` alone does not do: the default `exclude` still drops every `_` signal, so `openStream` sets `exclude: /(?!)/`, which matches nothing.

Signals, not headers. Datastar 1.0.4 builds a `@get`'s headers once, as the expression runs, and when it opens the stream again by itself, a retry after a broken connection or a hidden tab shown again, it sends those headers but reads the signals afresh. A build in a header stays the one the page reported first, though lith patched `$_build` meanwhile, so a hidden tab shown again after a deploy is refreshed every time; and a typed flag in a header stays false after the person typed, so their page is morphed. Datastar cancels a request by its method and the URL the expression names, before it adds the signals, so a stream opened again still aborts the old request; the hub ends a stream that a newer one with its `ID` replaces, too.

The stream handler reads the signals with `sse.ReadSignals(w, r, &page)`, which reads a GET's from its query parameter, into:

```go source=site/internal/docs/example/web/server.go
type pageState struct {
	Build string `json:"_build"`
	Dirty bool   `json:"_dirty"`
}
```

and passes them on:

```go source=site/internal/docs/example/web/server.go
	err = s.hub.Serve(w, r, live.Spec{
		ID: streamID(sid, tab), Views: views, WriteTick: tailor(), Reconnect: StreamID,
		Build: page.Build, Page: r.Header.Get("Page-Path"), Dirty: page.Dirty,
		Refresh: s.refresh,
	})
```

**Which build.** The default `Options.Build` is the same for every local build of a modified tree (the revision with `+dirty`), so a page loaded from one local build never refreshes on the next. An app can set `Build` to something that follows the content instead, such as a hash over every page's ETag: then a deploy that changes no page keeps the build, and its tabs get nothing at all.

**When it runs.** When `Spec.Refresh` is set and `Spec.Build` is not empty and differs from `hub.Build()`. A page loaded from the running build reports that build, and nothing happens. Otherwise lith calls `Refresh` and writes its events as the stream's first write, before the views' first frames. After those frames it patches the build signal (`live.Options.BuildSignal`, `_build` by default) to the running build in a signal patch of its own, never inside the morphed markup, so the next reconnect refreshes nothing. It patches the signal too when `Refresh` returns nil.

The board's `Refresh` renders `#content` from the page's path as the GET renders it, or, when the person typed a title, a notice that asks for a reload:

```go source=site/internal/docs/example/web/views.go
func (s *Server) refresh(ctx context.Context, rq live.RefreshRequest) ([][]byte, error) {
	if rq.Dirty {
		ev, err := patch(ctx, Update())
		return [][]byte{ev}, err
	}
	path, _, _ := strings.Cut(rq.Page, "?")
	rest, ok := strings.CutPrefix(path, "/boards/")
	id, err := strconv.ParseInt(rest, 10, 64)
	if !ok || err != nil {
		return nil, nil // not a board's page
	}
	return s.db.ViewValue(ctx, func(tx *sql.Tx) ([][]byte, error) {
		b, err := board.Load(ctx, tx, id)
		if errors.Is(err, board.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		lanes, err := renderLanes(ctx, b)
		if err != nil {
			return nil, err
		}
		ev, err := patch(ctx, Content(b, lanes))
		return [][]byte{ev}, err
	})
}
```

`RefreshRequest` carries the page's path, the build it reported, `Dirty` and the stream's `*http.Request`, for the language, the cookies and the app's own headers. `Refresh` reads through the app's own transactions, with a ctx that ends after `Options.RenderTimeout`.

**What a refresh may morph.** A container the app names, such as `#content` or `<main id="main">`, never `<body>`:

- The stream's element (`Spec.Reconnect`) stays outside it. A morph of it would run its `data-init` again and open a second stream. lith skips a refresh whose events hold an element with that id, logs it at warn and counts it (`Stats.RefreshesGuarded`). The page's signals and `#ops` stay outside too.
- Live views inside the container are fine: their first frames follow the refresh and win. The board's lanes, its presence and the tab's outcome render afresh twice, once in the refresh and once in their views' frames. A live region the refresh renders as a placeholder shows it for an instant before its frame follows, which does no harm.
- Signals declared in the refreshed markup use Datastar's `__ifmissing` (`data-signals:x__ifmissing`), so they add a signal the new build needs and leave the page's own values alone. A plain `data-signals` would overwrite them, the tab id and the pending ops among them.
- Elements with `data-init` or `data-on-intersect` inside the container keep stable ids and unchanged attributes, since a morph that leaves an element's attributes as they are runs nothing again; or they sit inside a live view, or carry `data-ignore-morph`. Stateful custom elements likewise. An attribute a signal drives keeps its value through the morph with `data-preserve-attr`, as the form's `readonly` and `aria-busy` do.
- A page with input the person typed (`Dirty`) gets a notice that asks for a reload, not a morph.
- A page whose scripts or stylesheets changed gets the notice too: a morph would put new markup under the old hashed CSS, or call modules the page never loaded. The template's page reports its assets version in the signal `$_assets`, which its stream handler hands to its `Refresh`, and that compares it:

```go source=template/internal/web/live.go
	switch {
	case rq.Dirty:
		msg = views.UpdateTyped
	case assets != s.assets:
		msg = views.UpdateAssets
	}
```

**Only a notice.** A `Refresh` that never morphs and always returns the notice that asks for a reload is a supported choice, not only the fallback for a page with typed input. It suits an app whose areas keep their state in plain signals and stateful elements, an editor or a board, where a morph would have to spare each of them: the person reloads when it suits them, and nothing changes under their hands meanwhile.

**A page the deploy did not change.** `Refresh` returns nil when the page is current, such as when the page reports its ETag in a signal of its own, which a refresh patches with the new content, and it matches the one the running build computes for it. A deploy that did not touch a page then costs its tabs only that check (`Stats.RefreshesUnchanged`), and the build signal is patched all the same.

**Pages without a stream.** A refresh needs a stream. An app that wants it on every page opens a stream on every page, with `Reconnect` and no views or only the tab's own: an idle stream that hibernates costs little ([Hibernation](/docs/guides/streams#hibernation)).

**After a deploy, all at once.** Every open tab reconnects within a few seconds of a deploy, and each one that a former build rendered calls `Refresh`. At most `Options.RefreshConcurrency` refreshes run at once, the hub's render `Workers` by default, each under `RenderTimeout`. The streams of one `Spec.Client` refresh one at a time, before they take a shared slot: the build is the client's to report, and a client that reports an old one on every stream it opens would otherwise hold every slot and leave other people's pages stale. A refresh that fails, runs past `RenderTimeout`, or finds no slot within `live.RefreshWait` (1 s) is skipped and logged, at most once every 10 s for each reason, and the stream goes on without it. Such a stream leaves the build signal as it was, so the page asks again on its next reconnect. prom exports the counts as `<namespace>_live_refreshes_total`, `_refreshes_unchanged_total` and `_refreshes_skipped_total{reason}`, with the reasons `error`, `timeout`, `busy` and `stream_element`.

`TestRefreshAfterDeploy` (`site/internal/docs/example/web`) loads a board, makes it a page of an old build and reconnects its stream in headless Chromium: `#content` is rendered afresh, the page keeps one stream and `$_build` becomes the running build. With a title typed, the page gets the notice and keeps the title. `TestRefreshAfterReopen` then hides and shows the refreshed tab twice and breaks its connection: the page reports the running build, is not refreshed again and keeps one stream; a page of an old build where the person typed gets the notice when it shows again, not the morph.

## What an action's answer carries

An action answers 204 with no body. What it changed reaches the page on the stream, as it reaches every other page, in the next tick. An answer that patched elements would race the next frame, which renders the same region from the database and overwrites it. A message for the person, a validation error among them, is an outcome in the tab's row or a notice on its stream, never part of the answer ([Commands](/docs/guides/commands#outcomes-notices-and-the-pages-own-words)).

An answer may patch a signal for a navigation, which concerns that tab alone and has no region to render into. Creating a board sends the page to the new board, and a retry must send it to the same one:

```go source=site/internal/docs/example/web/server.go
w.Header().Set("Content-Type", "text/event-stream")
w.Header().Set("Cache-Control", "no-store")
w.Write(sse.PatchSignals(fmt.Appendf(nil, `{"_goto":"/boards/%s"}`, op.Result()), false))
```

The page that posts it follows the signal with `data-effect="$_goto && location.assign($_goto)"`.

## Verbs and signals

Datastar sends a request's signals, or the `payload` it names, as the `datastar` query parameter for `@get` and `@delete`, and as a JSON body for every other verb. A lith page picks the verb by what the request does:

- `@get` opens the stream, and makes reads whose signals are all `_` and stay in the browser, but for the few a `filterSignals` names, such as the build the stream reports: the tab's id travels in the address. Datastar closes a `@get` while the tab is hidden and opens it again when the tab shows.
- `@query`, the HTTP QUERY method, makes a read that sends real signals in its body: a search, a filter, a measurement. It writes nothing, like a GET, and `edge.Compress` compresses its answer as it does a GET's. A `@query` stays open in a hidden tab, so one that opens a stream sets `openWhenHidden: false`.
- `@post` sends a command.

`sse.ReadSignals(w, r, &v)` reads the signals for any method: the query parameter of GET, HEAD and DELETE, the body of the others, at most 1 MiB (`sse.ReadSignalsLimit` sets another cap). A request without signals leaves `v` as it was; bad JSON and a body over the cap are errors, which a handler answers with 400. It reads the body to its end. Over HTTP/1.1, net/http notices that a client has left only once the body is read, or discarded when the answer's headers go out, so a handler that waits before it answers reads the signals first. Read them before `sse.Open` or `hub.Serve` too: the stream's headers discard the body, and `ReadSignals` then returns `sse.ErrBodyConsumed`.

datastar-go has a `ReadSignals(r, v)` with the same split between the address and the body. lith's takes the `ResponseWriter` as well, for `http.MaxBytesReader`: it caps the body where datastar-go reads any size, and it takes an empty body as no signals where datastar-go returns an error. Both read the body to its end.
