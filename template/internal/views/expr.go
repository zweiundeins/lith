// Package views renders the pages and the regions that streams patch. A
// shared region (Entries) holds nothing about the viewer: the same bytes go
// to every tab.
package views

import (
	"fmt"
	"strconv"
)

// signals are the page's, all local (_): an action sends only its payload,
// and the stream only the signals openStream names.
// $_ops holds the ops by key, here the form's alone ($_ops.sign): the
// pending op id, the newest op the tab's row acks and whether it was
// refused, the requests in flight and whether one must look again when
// they end. $_synced tells whether the stream's tab view has spoken since
// it connected. $_build and $_assets are the build and the assets version
// that rendered the page, and $_dirty whether the person has typed since
// the last signature: the stream reports them for a refresh after a
// deploy (web.refresh), which patches $_build once the page is current.
func signals(p Page) string {
	return fmt.Sprintf(`{_tab: %q, _build: %q, _assets: %q, _dirty: false, _slow: %s, _name: '', _message: '', _ops: {sign: {op: '', ack: '', refused: false, inflight: 0, recheck: false}}, _synced: false, _problem: ''}`, p.Tab, p.Build, p.Assets, slowLink)
}

// OpsID is the element that keeps the page's ops: hidden, outside every
// region a frame morphs, so it outlives the form, the menu or the dialog
// an op came from. Each op's request goes out from an element of its own
// inside it, which names the op's key (data-op-key), and its listeners
// follow every op by that key.
const OpsID = "ops"

// slowLink is true on a slow link, measured once as the page starts. Where
// the browser has navigator.connection (Chromium): data saving, or a link
// it calls 2g or 3g. Elsewhere, from Navigation Timing: the document's
// first byte more than 300 ms after its request, or under 100 KB/s.
const slowLink = `((c, n) => c ? c.saveData || ['slow-2g', '2g', '3g'].includes(c.effectiveType) : !!n && n.transferSize > 0 && (n.responseStart - n.requestStart > 300 || n.transferSize < 100 * (n.responseEnd - n.responseStart)))(navigator.connection, performance.getEntriesByType('navigation')[0])`

// openStream opens the tab's stream on the element with id "stream". On a
// slow link, measured as the page starts, it sends Save-Data: on
// (edge.SlowLink). Datastar cancels by the URL written here, without the
// signals, so a stream opened again aborts the old request. retry 'error'
// retries network errors and every answer from 400 to 599 and never gives
// up, which carries the page through a restart; a 204 or a 3xx stops it,
// and a stream the server ends stays closed: on a deploy the server ends
// it with sse.Reconnect, which runs this again. It marks the page out of
// sync until the tab view's first frame. For a refresh after a deploy
// (web.refresh) it reports the page's build, assets version and whether
// the person typed as signals, which Datastar reads afresh for every
// reopen, a retry or the tab shown again, where it repeats the first
// request's headers; the path, which never changes, goes in a header.
const openStream = `($_synced = false, @get('/stream?tab=' + $_tab, {headers: {'Page-Path': location.pathname + location.search, ...($_slow ? {'Save-Data': 'on'} : {})}, filterSignals: {include: /^_(build|assets|dirty)$/, exclude: /(?!)/}, retry: 'error', retryMaxCount: Infinity, retryMaxWait: 10000}))`

// typed marks the page dirty: a refresh after a deploy then asks for a
// reload instead of rendering the form afresh.
const typed = `$_dirty = true`

// retrying marks the page out of sync while Datastar opens the stream
// again after its connection broke.
const retrying = `evt.detail.el === el && evt.detail.type === 'retrying' && ($_synced = false)`

// hidden marks the page out of sync when the tab hides: Datastar closes
// the stream then and opens it again when the tab shows, without
// data-init and without retrying.
const hidden = `document.hidden && ($_synced = false)`

// reopen opens the stream again, as sse.Reconnect has the page do.
const reopen = `(s => s.replaceWith(s.cloneNode(true)))(document.getElementById('stream'))`

// behind asks the server to pace the tab's stream each time newest-frame.js
// reports that the page fell behind it (newest-frame-behind); /pace counts
// the report for app_live_newest_frame_behind_total too. It runs on
// <body>, where the event bubbles to: a @post from the stream's element
// would abort its stream.
const behind = `@post('/pace?tab=' + $_tab, {requestCancellation: 'disabled'})`

// act posts payload, a JavaScript object, to path as the op of key, unless
// an op of that key is pending. The tab's id and a new op id go in the
// Tab-Id and Op-Id headers, so the handler checks its rate limit and
// sends the tab a notice before it reads the body. Headers suit an op,
// unlike the stream: its request goes out from an element of its own in
// #ops, whose data-init holds this op's values, so a retry repeats the
// right ones. The element names the key, so the element the person acted
// on may go meanwhile. requestCancellation 'disabled': Datastar cancels a
// request when another goes to the same URL, anywhere on the page. The
// tab's stream brings the op's outcome, or a notice, back as
// $_ops[key].ack.
func act(key, path, payload string) string {
	return `((k, p) => {
	const e = $_ops[k];
	if (e.op && e.ack !== e.op) return;
	$_problem = '';
	const op = Date.now().toString(36) + Math.random().toString(36).slice(2, 7);
	e.op = op;
	e.refused = false;
	e.recheck = false;
	const c = document.createElement('i');
	c.dataset.opKey = k;
	c.setAttribute('data-init', "@post('` + path + `', {headers: " + JSON.stringify({'Tab-Id': $_tab, 'Op-Id': op}) + ", payload: " + JSON.stringify(p) + ", requestCancellation: 'disabled'})");
	document.getElementById('` + OpsID + `').append(c);
})('` + key + `', ` + payload + `)`
}

// sign sends the form as the op "sign", which leaves nothing typed to lose.
var sign = `$_name.trim() && $_message.trim() && ($_dirty = false, ` + act("sign", "/cmd/sign", "{name: $_name, message: $_message}") + `)`

// pending is true from the click until the stream brings the outcome.
const pending = `!!$_ops.sign.op && $_ops.sign.ack !== $_ops.sign.op`

// busy is pending for aria-busy: data-attr writes true as an empty value,
// which aria-busy reads as false.
const busy = `(` + pending + `) ? 'true' : false`

// fetched follows the requests of every op, by the key their element
// names. It counts those in flight, per key. It ends an op when its
// request never got through (retries-failed), and when it answered a
// status that comes without a notice and without a stream to decide: a
// 4xx other than 409 and 429 is a request no page of ours sends. A 429 or
// a 5xx comes with a notice, or as the stream goes away, and a 409 says
// the tab's stream is not open: those it leaves to the stream, whose
// reconnect decides. When the last request of a
// key ends after the stream connected again without its ack (patched),
// it opens the stream once more, so that a fresh frame of the tab's row
// decides.
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
// notice) brings settles, and the message empties once it was accepted;
// when the stream connected again ($_synced turns true) and the tab's row
// does not ack an op, the op ends in the page's own words, unless a
// request of its key is in flight: nothing is decided then, and fetched
// looks again when it ends. What the person typed stays. It is one
// function call: Datastar splits a data-on-signal-patch at its
// semicolons.
const patched = `(() => {
	for (const k in patch._ops || {}) {
		const e = $_ops[k];
		if (!!e.op && e.ack === e.op) {
			e.op = '';
			if (k === 'sign' && !e.refused) $_message = '';
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

func count(n int) string {
	switch n {
	case 0:
		return "No entries yet"
	case 1:
		return "1 entry"
	}
	return strconv.Itoa(n) + " entries"
}
