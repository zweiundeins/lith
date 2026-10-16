package town

import (
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
)

// The Datastar expressions of the demo. Its signals live under $_town,
// local to the browser: an action sends only its payload. data-on,
// data-init and data-effect run their text as the body of a function.

// signals are the demo's own, declared on its stage. tool is the palette's
// choice; want the slider's value, mine the number of the page's last seek;
// at and seq the position the server has the tab at and the number of the
// seek that put it there (the tab's view); first and head the slider's ends
// (the head view); op the change waiting for its outcome on tile ptile, ack
// and ok the tab's last outcome; hello a new stream's first frame.
func signals(b Bounds) string {
	h := strconv.FormatInt(b.Head, 10)
	return `{_town: {tool: 'house', want: ` + h + `, mine: 0, seq: 0, at: 0, first: ` + strconv.FormatInt(b.First, 10) + `, head: ` + h +
		`, op: '', ptile: -1, ack: '', ok: false, problem: '', hello: false}}`
}

// seek sends the slider's position: at the right end, now (0). Each seek
// has a number, so that the server drops one the network delayed past a
// newer one. requestCancellation 'disabled': Datastar would cancel the
// seek in flight, and its number with it.
const seek = `const want = +$_town.want;
$_town.mine++;
@post('/cmd/town/seek', {payload: {tab: $_tab, at: want >= $_town.head ? 0 : want, seq: $_town.mine}, requestCancellation: 'disabled'})`

// drag sends the slider's position once every Throttle at most, Datastar's
// throttle at its leading edge and at its trailing one, so that the
// position where the thumb comes to rest goes too: a position a tick. It
// reads the position from the slider itself, which data-bind may not have
// handed to the signal yet.
func drag() templ.Attributes {
	return templ.Attributes{"data-on:input__throttle." + ms(Throttle) + ".trailing": "$_town.want = +el.value;\n" + seek}
}

func ms(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
}

// now goes to the slider's end, which is live.
const now = `$_town.want = $_town.head;
` + seek

// edges keeps the slider's ends on the head view's positions, and the
// thumb at the end while the tab is live and no seek is under way: then
// new events move it along. The ends go first, so that the value is not
// clamped to the old end.
const edges = `el.min = $_town.first;
el.max = $_town.head;
if ($_town.at === 0 && $_town.mine === $_town.seq) $_town.want = $_town.head`

// seekFailed ends a seek that never reached the server (the rate limit,
// the network gone): no frame will answer it.
const seekFailed = `if (!['error', 'retries-failed'].includes(evt.detail.type) || !evt.detail.el.closest('.town-time')) return;
$_town.mine = $_town.seq;
$_town.problem = evt.detail.argsRaw?.status === '429' ? 'The slider moved too fast for the server. Drag again.' : 'That position did not reach the server. Drag again.'`

// place sends a change of the tile clicked, with the tool and the tile's
// version on the page, unless the tab shows the past or a change is
// under way. The tile waits, marked pending, until the tab's view brings
// the change's outcome back (settle).
var place = expr(`const b = evt.target.closest('[data-t]');
if (!b || $_town.at !== 0 || $_town.op) return;
$_town.problem = '';
$_town.ptile = +b.dataset.t;
$_town.op = Date.now().toString(36) + Math.random().toString(36).slice(2, 7);
@post('/cmd/town/place', {payload: {tab: $_tab, op: $_town.op, tile: +b.dataset.t, kind: $_town.tool, seen: +b.dataset.v}, requestCancellation: 'disabled'})`)

// keys move the focus from tile to tile with the arrow keys.
var keys = expr(`const b = evt.target.closest('[data-t]');
if (!b || !evt.key.startsWith('Arrow')) return;
const t = +b.dataset.t, c = t % COLS, k = evt.key;
const to = k === 'ArrowLeft' ? (c > 0 ? t - 1 : -1) : k === 'ArrowRight' ? (c < COLS - 1 ? t + 1 : -1) : k === 'ArrowUp' ? t - COLS : t + COLS;
if (to < 0 || to >= TILES) return;
evt.preventDefault();
el.querySelector('[data-t="' + to + '"]').focus()`)

// settle ends the pending change once its outcome has come.
const settle = `$_town.op && $_town.ack === $_town.op && ($_town.op = '', $_town.ptile = -1)`

// placeFailed ends a change whose request failed for good.
const placeFailed = `if (evt.detail.el !== el || !$_town.op || !['error', 'retries-failed'].includes(evt.detail.type)) return;
$_town.problem = evt.detail.argsRaw?.status === '429' ? 'Too many changes at once. Wait a moment and try again.' : 'That change did not reach the server. Try again in a moment.';
$_town.op = '';
$_town.ptile = -1`

// greet answers a stream's first frame: the server then watches the town
// at the tab's position on the new stream. A refusal is tried again.
const greet = `if (!$_town.hello) return;
$_town.hello = false;
@post('/cmd/town/follow', {payload: {tab: $_tab}, retry: 'error', retryMaxCount: 5})`

// waiting is the style of the tile whose change waits for its outcome, set
// by its number from the signals, outside the region the stream morphs.
const waiting = `$_town.ptile < 0 ? '' : '#town-map [data-t="' + $_town.ptile + '"]{--town-wait:1}'`

// posLabel is the slider's position as the person drags it: their own input,
// shown at once.
const posLabel = `+$_town.want >= $_town.head ? 'Now' : 'Position ' + (+$_town.want).toLocaleString('en')`

// badge has the stage's badge say "past" while the tab shows the past:
// its stream is open, but what the stage shows is not live.
const badge = `$_badge = $_town.at !== 0 ? 'past' : ''`

// pending is true while a seek waits for the server.
const pending = `$_town.mine !== $_town.seq ? 'true' : false`

func expr(s string) string {
	return strings.NewReplacer("COLS", strconv.Itoa(Cols), "TILES", strconv.Itoa(Tiles)).Replace(s)
}
