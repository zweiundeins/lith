package checkboxes

import (
	"strconv"
	"strings"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// The Datastar expressions of the demo. Its signals live under
// $_checkboxes, in the browser only: an action sends its payload. The
// handlers sit on the stage, which no frame morphs, and catch what
// bubbles up from the boxes.

// The grid's size before the page measures it: two tiles to a row, boxes
// 20px apart. chrome is a tile's head and the space under it, in px
// (--cb-chrome in checkboxes.css).
const (
	defaultCols  = 2
	defaultPitch = 20
	chrome       = 34
	defaultItem  = 16*defaultPitch + chrome
)

// signals are the demo's own, declared on the stage: the grid's geometry,
// the jump field, the box it asked for, a count of tiles and windows that
// arrived, the last problem to report, hello, which a stream's first
// frame sets to ask for the window (greet), the newest click and the
// newest the server took (op, ack), and what demo.Pending keeps.
var signals = `{_checkboxes: {cols: ` + strconv.Itoa(defaultCols) + `, pitch: ` + strconv.Itoa(defaultPitch) + `, item: ` + strconv.Itoa(defaultItem) + `, jump: '', target: 0, moved: 0, problem: '', hello: false, op: '', ack: '', synced: false, inflight: 0, recheck: false}}`

// clicks are the boxes that wait: the newest click until the tab's clicks
// view acks it (op ids grow with time, so a newer one acks the older ones
// too). They end on that, a request that never got through, or a stream
// that connects again without the ack, never on a clock.
var clicks = func() demo.Pending {
	p := demo.NewPending("_checkboxes")
	p.Waiting = "$_checkboxes.op !== '' && !($_checkboxes.ack >= $_checkboxes.op)"
	p.End = "el.querySelectorAll('.cb-boxes > input:indeterminate').forEach((b) => (b.indeterminate = false)), $_checkboxes.op = ''"
	p.Down = "That click did not reach the server. Try again once the connection is back."
	p.Lost = "That click did not go through. Try again."
	return p
}()

// acked ends the pending state of the boxes whose clicks the server took,
// when the ack changes: a frame that changes nothing (someone set the box
// back in the same tick) ends it too. With lost, it is the stage's
// data-on-signal-patch.
const acked = `patch._checkboxes?.ack !== undefined && (
	el.querySelectorAll('.cb-boxes > input:indeterminate').forEach((b) => b.cbOp <= $_checkboxes.ack && (b.indeterminate = false)),
	$_checkboxes.op !== '' && $_checkboxes.ack >= $_checkboxes.op && ($_checkboxes.op = ''))`

// onPatch is the stage's data-on-signal-patch, kept to the changes of
// the ack and of synced by patchFilter.
var onPatch = acked + ";\n" + clicks.LostExpr()

const patchFilter = `{include: /^_checkboxes\.(ack|synced)$/}`

// measure fits the grid to the width it has, on load and on resize: two
// tiles to a row from 600px, boxes 16 to 24px apart. The tiles take the
// pitch from --cb-pitch, the list its row height and tiles per row from
// attributes that follow these signals.
const measure = `const w = el.querySelector('.cb__frame').clientWidth, cols = w >= 600 ? 2 : 1;
const chrome = parseFloat(getComputedStyle(el).getPropertyValue('--cb-chrome')) || 34;
$_checkboxes.pitch = Math.max(16, Math.min(24, Math.floor((w / cols - 12) / 16)));
$_checkboxes.cols = cols;
$_checkboxes.item = 16 * $_checkboxes.pitch + chrome`

// flip sends a click on a box as the state the box should take, the
// opposite of what it shows, with an op id, and marks the box pending
// until the frame that flips it (settle) or the ack of its op (acked).
var flip = expr(`const b = evt.target;
if (!b.matches('.cb-boxes > input')) return;
const n = +b.closest('[data-chunk]').dataset.chunk * CHUNK + Array.prototype.indexOf.call(b.parentNode.children, b);
// The browser has toggled the box by now: preventDefault puts it back,
// so it keeps showing what the server said.
evt.preventDefault();
const want = b.checked, sent = (b.cbSent || 0) + 1;
b.cbSent = sent;
$_checkboxes.problem = '';
$_checkboxes.op = b.cbOp = Date.now().toString(36) + Math.random().toString(36).slice(2, 7);
// Pending: indeterminate, unless the frame came first.
setTimeout(() => b.checked !== want && b.cbSent === sent && (b.indeterminate = true));
@post('/cmd/checkboxes/set', {payload: {tab: $_tab, op: $_checkboxes.op, box: n, checked: want}, requestCancellation: 'disabled'})`)

// settle ends a box's pending state when a frame changes it: Datastar
// fires datastar-prop-change on an input whose checked state its morph
// set.
const settle = `evt.target.indeterminate = false`

// fetched follows the clicks' requests (demo.Pending.Fetched).
var fetched = clicks.Fetched()

// setWindow tells the server which tiles the list wants. The server
// stores the window and moves the tab's stream to its chunks; the tab's
// view sends the list at its new offset. A newer request cancels the one
// in flight, as Datastar does for the same URL.
const setWindow = `@post('/cmd/checkboxes/window', {payload: {tab: $_tab, offset: evt.detail.offset, count: evt.detail.count, cols: $_checkboxes.cols}})`

// greet answers a stream's first frame with the window the list shows,
// so that the new stream watches its chunks. A refusal (the rate, a
// restart) is tried again.
const greet = `if (!$_checkboxes.hello) return;
$_checkboxes.hello = false;
const host = document.getElementById('checkboxes-grid');
@post('/cmd/checkboxes/window', {payload: {tab: $_tab, offset: +host.getAttribute('offset'), count: host.children.length, cols: $_checkboxes.cols}, retry: 'error', retryMaxCount: 5})`

// jump scrolls the list to the tile of the box asked for; focusTarget
// gives the box the focus once the tile is in its place.
var jump = expr(`const n = Math.round(+$_checkboxes.jump);
if (!(n >= 1 && n <= BOXES)) {
	$_checkboxes.problem = 'Pick a box from 1 to GROUPED.';
	return;
}
$_checkboxes.problem = '';
$_checkboxes.target = n;
document.getElementById('checkboxes-grid').scrollToIndex?.(Math.floor((n - 1) / CHUNK));
$_checkboxes.moved++`)

// focusTarget gives the box asked for the focus as soon as its tile sits
// in its place in the list. It runs whenever a tile or a window arrives
// ($_checkboxes.moved), while a box is asked for.
var focusTarget = expr(`const n = $_checkboxes.target - 1, moved = $_checkboxes.moved;
if (n < 0) return;
const c = Math.floor(n / CHUNK), host = document.getElementById('checkboxes-grid'), tile = document.getElementById('checkboxes-tile-' + c);
if (!tile || tile.parentElement.parentElement !== host) return;
if (+host.getAttribute('offset') + Array.prototype.indexOf.call(host.children, tile.parentElement) !== c) return;
// The list has not laid out its new window yet: no scrolling to the box.
const box = tile.querySelector('.cb-boxes').children[n % CHUNK];
requestAnimationFrame(() => box?.focus({focusVisible: true, preventScroll: true}));
$_checkboxes.target = 0`)

// arrows moves the focus between the boxes: left and right along them,
// up and down a row of 16, and on into the next tile of the window, the
// one beside or the one above or below.
var arrows = expr(`const b = evt.target, d = {ArrowLeft: -1, ArrowRight: 1, ArrowUp: -16, ArrowDown: 16}[evt.key];
if (!d || !b.matches('.cb-boxes > input')) return;
const tiles = [...document.querySelectorAll('#checkboxes-grid .cb-boxes')], box = b.parentNode, i = Array.prototype.indexOf.call(box.children, b), k = tiles.indexOf(box);
let to = box.children[i + d];
if (Math.abs(d) === 1 && !to) {
	const t = tiles[k + d];
	to = t && (d > 0 ? t.firstElementChild : t.lastElementChild);
} else if (Math.abs(d) === 16 && (i + d < 0 || i + d >= box.children.length)) {
	const t = tiles[k + Math.sign(d) * $_checkboxes.cols];
	to = t && t.children[Math.min(i % 16 + (d > 0 ? 0 : t.children.length - 16), t.children.length - 1)];
}
if (!to) return;
evt.preventDefault();
to.focus()`)

// expr fills in the grid's numbers.
func expr(s string) string {
	return strings.NewReplacer("CHUNK", strconv.Itoa(ChunkSize), "BOXES", strconv.Itoa(Boxes), "GROUPED", grouped(Boxes)).Replace(s)
}
