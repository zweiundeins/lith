package web

import (
	"fmt"
	"strconv"

	"github.com/zweiundeins/lith/site/internal/docs/example/board"
)

// The page's Datastar expressions. Its signals start with _, so they stay
// in the browser: an action sends only its payload.

// StreamID is the element whose data-init opens the tab's stream: on a
// deploy, the stream's last event (live.Spec.Reconnect) replaces it with
// a copy, and the copy opens the stream again on the next process.
const StreamID = "stream"

// OpsID is the element that keeps the page's ops: hidden, outside every
// region a frame morphs, so it outlives the form, the menu or the card an
// op came from. Each op's request goes out from an element of its own
// inside it, which names the op's key (data-op-key), and its listeners
// follow every op by that key.
const OpsID = "ops"

// signals are the page's: the tab's id, new for every document, the add
// form's title; the ops by key ($_ops.add, $_ops['card-7']): the pending
// op id, the newest op the tab's row acks and whether it was refused, the
// requests in flight and whether one must look again when they end;
// whether the stream's tab view has spoken since it connected; the
// heartbeat's watch; and, for a refresh after a deploy, the build that
// rendered the page and whether the person typed since the last card.
func signals(tab, build string, p board.Prefs) string {
	return fmt.Sprintf(`{_tab: %q, _title: '', _ops: {add: {op: '', ack: '', refused: false, inflight: 0, recheck: false}}, _synced: false, _problem: '', _compact: %t, _ping: 0, _beat: 0, _build: %q, _dirty: false}`, tab, p.Compact, build)
}

// openStream opens the tab's stream, and marks the page out of sync until
// the tab view's first frame. retry 'error' retries network errors and
// every answer from 400 to 599 without end, which carries the page
// through a restart; a 204 or a 3xx stops it, and a stream the server
// ended stays closed. Its headers report the page's build, path and
// whether the person typed, for a refresh (Server.refresh); headers, so
// that the URL stays the same and a stream opened again aborts the old
// request.
func openStream(b int64) string {
	return "($_synced = false, @get('/boards/" + strconv.FormatInt(b, 10) + "/stream?tab=' + $_tab, {headers: {'Page-Build': $_build, 'Page-Path': location.pathname + location.search, 'Page-Dirty': String($_dirty)}, retry: 'error', retryMaxCount: Infinity, retryMaxWait: 10000}))"
}

// typed marks the page dirty: a refresh asks for a reload instead.
const typed = `$_dirty = true`

// retrying marks the page out of sync while Datastar opens the stream
// again after its connection broke.
const retrying = `evt.detail.el === el && evt.detail.type === 'retrying' && ($_synced = false)`

// hidden marks the page out of sync when the tab hides: Datastar closes
// the stream then and opens it again when the tab shows, without
// data-init and without retrying.
const hidden = `document.hidden && ($_synced = false)`

// reopen opens the stream again, as sse.Reconnect has the page do.
const reopen = `(s => s.replaceWith(s.cloneNode(true)))(document.getElementById('` + StreamID + `'))`

// beat notes a sign of life: the hub patches $_ping every 20 s
// (live.Options.HeartbeatSignal), frames or not.
const beat = `$_ping, $_beat = Date.now()`

// watchdog opens the stream again when no heartbeat came for 50 s: a
// connection that went quiet without closing, as a laptop's after sleep.
func watchdog(b int64) string {
	return "Date.now() - $_beat > 50000 && ($_beat = Date.now(), " + openStream(b) + ")"
}

// act posts the action of an op key, a JavaScript expression, with a new
// op id, unless an op of that key is pending. The request goes out from an
// element of its own in #ops, which names the key, so the element the
// person acted on may go meanwhile. requestCancellation 'disabled':
// Datastar cancels a request when another goes to the same URL, anywhere
// on the page, and two cards' moves would cancel each other. The tab's
// stream brings the op's outcome, or a notice, back as $_ops[key].ack.
func act(key, path, payload string) string {
	return `((k, p) => {
	const e = $_ops[k];
	if (e && e.op && e.ack !== e.op) return;
	$_problem = '';
	const op = Date.now().toString(36) + Math.random().toString(36).slice(2, 7);
	$_ops[k] = {op: op, ack: e?.ack ?? '', refused: false, inflight: e?.inflight ?? 0, recheck: false};
	const c = document.createElement('i');
	c.dataset.opKey = k;
	c.setAttribute('data-init', "@post('` + path + `', {payload: " + JSON.stringify({tab: $_tab, key: k, op: op, ...p}) + ", requestCancellation: 'disabled'})");
	document.getElementById('` + OpsID + `').append(c);
})(` + key + `, {` + payload + `})`
}

// add adds a card with the title the form holds, and leaves nothing typed
// to lose.
func add(lane int64) string {
	return "$_title.trim() && ($_dirty = false, " + act("'add'", "/cmd/add", "lane: "+strconv.FormatInt(lane, 10)+", title: $_title") + ")"
}

// onBoard handles a click on a card's button: one with data-lane moves the
// card on, with the version the page shows (a card that moved meanwhile
// stays, and the tab hears why), as the op of the card's key; the other
// opens the card's panel.
var onBoard = `evt.target.dataset.card && (evt.target.dataset.lane
	? ` + act("'card-' + evt.target.dataset.card", "/cmd/move", "card: +evt.target.dataset.card, lane: +evt.target.dataset.lane, seen: +evt.target.dataset.seen") + `
	: ` + panel("+evt.target.dataset.card") + `)`

// pending is true while an op of the key waits for its ack.
func pending(key string) string {
	e := `$_ops['` + key + `']`
	return `!!` + e + `?.op && ` + e + `.ack !== ` + e + `.op`
}

// busy is pending for aria-busy: data-attr writes true as an empty value,
// which aria-busy reads as false.
func busy(key string) string { return `(` + pending(key) + `) ? 'true' : false` }

// waitingCards fades the cards whose moves wait for their acks, from the
// signals, outside the regions the stream morphs.
const waitingCards = `Object.keys($_ops).filter(k => k.startsWith('card-') && $_ops[k].op && $_ops[k].ack !== $_ops[k].op).map(k => '#' + k + '{opacity:.5}').join('')`

// fetched follows the requests of every op, by the key their element
// names. It counts those in flight, per key. It ends an op when its
// request never got through (retries-failed), and when it answered a
// status that comes without a notice: a 4xx other than 409 and 429 is a
// check no page of ours fails, and no notice ends it. A 429 or a 5xx comes
// with a notice, or as the stream goes away, and a 409 (an app's answer to
// a tab without a stream) waits for the stream: those it leaves to the
// stream, whose reconnect decides. When
// the last request of a key ends after the stream connected again without
// its ack (patched), it opens the stream once more, so that a fresh frame
// of the tab's row decides.
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

// patched ends ops from the stream: an op whose ack the tab's view (or a
// notice) brings settles, and the title empties once a card was added;
// when the stream connected again ($_synced turns true) and the tab's row
// does not ack an op, the op ends in the page's own words, unless a
// request of its key is in flight: nothing is decided then, and fetched
// looks again when it ends. The person's input stays. It is one function
// call: Datastar splits a data-on-signal-patch at its semicolons.
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

// opsPatches keeps patched to the acks and to $_synced: a click alone
// never runs it.
const opsPatches = `{include: /^(_ops\.[^.]+\.ack|_synced)$/}`

// panel opens a card's panel in this tab, or closes it with card 0.
func panel(card string) string {
	return "@post('/cmd/panel', {payload: {tab: $_tab, card: " + card + "}})"
}

// compact sets the session's preference, for every tab of the session.
const compact = `@post('/cmd/compact', {payload: {tab: $_tab, compact: !$_compact}})`

func id(n int64) string { return strconv.FormatInt(n, 10) }

// people says how many tabs show the board.
func people(n int) string {
	if n == 1 {
		return "1 tab on this board"
	}
	return strconv.Itoa(n) + " tabs on this board"
}

// firstLane is the lane the add form adds to.
func firstLane(b board.Board) int64 {
	if len(b.Lanes) == 0 {
		return 0
	}
	return b.Lanes[0].ID
}
