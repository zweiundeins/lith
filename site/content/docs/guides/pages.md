---
title: Pages and Datastar
nav: Pages
headline: Pages with Datastar: regions, tab state, op ids
summary: The document a GET renders, regions morphed whole, a tab's state in SQLite, an action that stays pending until its op id comes back, and a page that reconnects by itself after a deploy.
description: The document a GET renders, regions morphed whole, tab state in SQLite, actions pending until their op id returns, and reconnecting after a deploy.
order: 5
---

A lith page is a document that a GET renders from one read transaction, and a stream that patches it from then on. [Datastar](https://data-star.dev) runs in the browser: it sends the actions, keeps the page's signals, and morphs the HTML that arrives on the stream into the page by the elements' ids. The [Datastar notes](/docs/datastar) list what goes wrong with it, and why.

## The document

The board's document holds the page's signals, the element that opens the stream, the add form and the board:

```templ source=site/internal/docs/example/web/board.templ
<main data-signals={ signals(p.Tab, p.Prefs) } data-class:compact="$_compact">
	<div id={ StreamID } hidden data-init={ "$_beat = Date.now(); " + openStream(p.Board.ID) } data-effect={ beat } data-on-interval__duration.10s={ watchdog(p.Board.ID) }></div>
	<h1>{ p.Board.Name }</h1>
	@Presence(0)
	<button type="button" data-on:click={ compact } data-text="$_compact ? 'Show details' : 'Compact'">Compact</button>
	<form data-on:submit__prevent={ add(firstLane(p.Board)) } data-on:datastar-fetch={ failed } data-effect={ settle }>
		<input data-bind="_title" maxlength={ strconv.Itoa(board.MaxTitle) } required aria-label="New card" data-attr:readonly={ pending }/>
		<button type="submit" data-attr:aria-busy={ busy }>Add</button>
	</form>
	<p class="problem" role="alert" data-show="$_problem" data-text="$_problem"></p>
	@Outcome(board.TabState{})
	@Board(p.Lanes)
	@NoPanel()
</main>
```

The expressions live in Go, in `expr.go`, beside the templates that use them. Every signal starts with `_`: such signals stay in the browser, and an action sends only the payload it names. The tab's id, `$_tab`, is new for every document, so two tabs of one browser are two tabs to the server; the server never stores it until the tab acts.

```go source=site/internal/docs/example/web/expr.go
func signals(tab string, p board.Prefs) string {
	return fmt.Sprintf(`{_tab: %q, _title: '', _op: '', _ack: '', _refused: false, _problem: '', _compact: %t, _ping: 0, _beat: 0}`, tab, p.Compact)
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
	<div id="board" data-on:click={ onBoard } data-on:datastar-fetch={ failed }>
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
	Op      string `json:"op,omitempty"`
	Refusal string `json:"refusal,omitempty"`
	Panel   int64  `json:"panel,omitempty"`
}
```

It is a row, not a map in the server's memory, so it survives a deploy: the page reconnects to the next process, and the stream's handler finds the open panel in the row ([Views and the hub](/docs/guides/views#adding-a-view-to-an-open-stream)). A map of tabs in memory is empty after every restart. The sweep deletes rows of tabs that stayed quiet for a week.

A preference that should outlive the page, and apply in every tab, is a row keyed by the session: the board's compact mode. The GET renders it into the document, so the page never flashes the other setting, and the session's view patches it into every open tab when it changes.

What only the browser needs, what someone is typing, an open menu, stays in `_` signals and never reaches the server.

## Op ids and the pending state

A click shows the user's own input at once and marks it pending. The outcome, a new card or a refusal, comes with the next frame, on the stream. The page never shows an outcome before the server has decided it, so there is nothing to roll back.

1. The action makes an op id and posts it with its payload, unless an op is pending:

```go source=site/internal/docs/example/web/expr.go
func act(path, payload string) string {
	return `$_op === '' && ($_problem = '', $_op = Date.now().toString(36) + Math.random().toString(36).slice(2, 7),
	@post('` + path + `', {payload: {tab: $_tab, op: $_op, ` + payload + `}}))`
}
```

2. While `$_op` holds an id, the form is pending: the input is read-only and the button has `aria-busy`, for screen readers and for the stylesheet:

```go source=site/internal/docs/example/web/expr.go
// pending is true from the click until the stream brings the outcome.
const pending = `$_op !== ''`

// busy is pending for aria-busy: data-attr writes true as an empty value,
// which aria-busy reads as false.
const busy = `$_op !== '' ? 'true' : false`
```

3. The handler wraps the command in `writer.Once` with the op id and answers 204 ([Commands](/docs/guides/commands#op-ids-and-retries)). The command records the op and any refusal in the tab's row.

4. The tab's view renders the row and patches `$_ack` with the op id. The form's effect ends the pending state when the ack matches, and empties the field when the card was added:

```go source=site/internal/docs/example/web/expr.go
// settle ends the pending state when the op's outcome arrives, and empties
// the title once a card was added.
const settle = `$_op !== '' && $_ack === $_op && (!$_refused && ($_title = ''), $_op = '')`
```

5. A request that fails, refused by a rate limit or lost with the network, brings no outcome, so the page ends the pending state itself and says so:

```go source=site/internal/docs/example/web/expr.go
const failed = `evt.detail.el === el && ['error', 'retries-failed'].includes(evt.detail.type) && ($_op = '', $_problem = 'That did not go through. Try again in a moment.')`
```

Datastar's fetch events reach the listeners of other elements too, so `evt.detail.el === el` keeps each listener to the requests of its own element.

## A refusal next to where the user acted

The tab's view renders the refusal into `#outcome`, beside the form, in this tab only:

```templ source=site/internal/docs/example/web/board.templ
// Outcome is the tab's own region: why its last action changed nothing.
templ Outcome(s board.TabState) {
	<p id="outcome" role="status">{ s.Refusal }</p>
}
```

Other tabs see the board change, or not change, and nothing else.

## Reconnecting after a deploy

The stream opens from an element with an id, with Datastar's `retry: 'error'`:

```go source=site/internal/docs/example/web/expr.go
func openStream(b int64) string {
	return "@get('/boards/" + strconv.FormatInt(b, 10) + "/stream?tab=' + $_tab, {retry: 'error', retryMaxCount: Infinity, retryMaxWait: 10000})"
}
```

The stream's `Spec.Reconnect` names that element. When the server stops for a deploy, the hub ends every stream, and each one's last event, `sse.Reconnect`, replaces the element with a copy of itself: the copy's `data-init` runs and opens the stream again. During the restart the old process answers 503 while it stops, and the proxy 502 while no process listens; `'error'` retries both, and `retryMaxCount: Infinity` keeps trying until the new process answers. The new stream sends every view's current frame, so the page catches up with what changed meanwhile.

- Datastar's default, `retry: 'auto'`, retries network errors only. It gives up at the proxy's 502, and never reopens a stream the server ended cleanly, so a deploy leaves the page stale without a sign.
- `retry: 'always'` reopens every stream that ends, including one the server ended on purpose: a stream that a newer stream of the same tab replaced, say.
- The element does nothing but open the stream: every deploy runs its `data-init` again, so anything else made there would be made again.

## What an action's answer carries

An action answers 204 with no body. What it changed reaches the page on the stream, as it reaches every other page, in the next tick. An answer that patched elements would race the next frame, which renders the same region from the database and overwrites it.

An answer may patch signals of the tab that sent it, for what concerns that tab alone. Creating a board sends the page to the new board, and a retry must send it to the same one:

```go source=site/internal/docs/example/web/server.go
w.Header().Set("Content-Type", "text/event-stream")
w.Header().Set("Cache-Control", "no-store")
w.Write(sse.PatchSignals(fmt.Appendf(nil, `{"_goto":"/boards/%s"}`, op.Result()), false))
```

The page that posts it follows the signal with `data-effect="$_goto && location.assign($_goto)"`.
