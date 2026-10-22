package board

import (
	"strconv"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// The Datastar expressions of the demo. Its signals live under $_board,
// local to the browser: a move sends only its payload. data-on, data-init
// and data-effect run their text as the body of a function; data-text
// returns its last statement.
//
// Nothing here moves a card. A drag or the keyboard picks a place, and the
// page shows where the card would go; the move goes to the server, the card
// waits there, marked pending, and the board's next frame moves it.

// signals are the demo's own, declared on its stage. v is the board's
// version on the page; drag the card in hand (0: none) by mode, 'pointer'
// or 'keys', picked up at version seen from lane from, place at; lane and
// index are where it would go (lane -1: nowhere), n how many other cards
// that lane has; op is the move waiting for its outcome, pcard its card;
// ack, ackv and ok are the tab's last outcome; nudge is a refused card for
// a moment; say is the live region's text; synced, inflight and recheck
// are what demo.Pending keeps.
func signals(version int64) string {
	return `{_board: {v: ` + strconv.FormatInt(version, 10) + `, drag: 0, mode: '', seen: 0, from: -1, at: 0, lane: -1, index: 0, n: 0, op: '', pcard: 0, ack: '', ackv: 0, ok: false, nudge: 0, say: '', problem: '', synced: false, inflight: 0, recheck: false}}`
}

// moving is the pending move: it ends on its outcome (settle), a request
// that never got through, or a stream that connects again without its
// ack, never on a clock.
var moving = func() demo.Pending {
	p := demo.NewPending("_board")
	p.End = "$_board.op = '', $_board.pcard = 0, $_board.drag || el.bd.aim(-1, 0)"
	p.Down = "That move did not reach the server. Try again in a moment."
	p.Lost = "That move did not go through. Try again."
	return p
}()

// controller holds the drag and the keyboard's helpers, on the stage
// element (el.bd), for the handlers below. They read the page: the lanes
// carry data-lane and data-name, the cards data-card, data-version and
// data-title. On a narrow board a lane is a row of cards that wraps, and
// board.css says so with --bd-flow: a place is then before the first card
// below the pointer, or beside it to the right.
const controller = `el.bd = {press: null}
const B = el.bd
B.lane = (n) => el.querySelector('[data-lane="' + n + '"]')
B.card = (id) => document.getElementById('board-card-' + id)
B.laneOf = (c) => +c.closest('[data-lane]').dataset.lane
B.others = (lane) => [...B.lane(lane).querySelectorAll('[data-card]')].filter((c) => +c.dataset.card !== ($_board.drag || $_board.pcard))
B.say = (text) => { $_board.say = $_board.say === text ? text + '\u00a0' : text }
B.place = (lane, index, n) => B.lane(lane).dataset.name + ', ' + (index + 1) + ' of ' + (n + 1)
B.aim = (lane, index) => {
	if (lane < 0) { $_board.lane = -1; return }
	const n = B.others(lane).length
	$_board.index = Math.max(0, Math.min(index, n))
	$_board.n = n
	$_board.lane = lane
}
B.aimAt = (x, y) => {
	const box = el.querySelector('.bd-lanes').getBoundingClientRect()
	if (x < box.left - 48 || x > box.right + 48 || y < box.top - 64 || y > box.bottom + 64) { B.aim(-1, 0); return }
	let lane = 0, best = Infinity
	for (const l of el.querySelectorAll('[data-lane]')) {
		const r = l.getBoundingClientRect()
		const dx = x < r.left ? r.left - x : x > r.right ? x - r.right : 0
		const dy = y < r.top ? r.top - y : y > r.bottom ? y - r.bottom : 0
		if (dx * dx + dy * dy < best) { best = dx * dx + dy * dy; lane = +l.dataset.lane }
	}
	const flow = getComputedStyle(B.lane(lane)).getPropertyValue('--bd-flow').trim() === '1'
	const cards = B.others(lane)
	const i = cards.findIndex((c) => {
		const r = c.getBoundingClientRect()
		return flow ? y < r.top || (y < r.bottom && x < r.left + r.width / 2) : y < r.top + r.height / 2
	})
	B.aim(lane, i < 0 ? cards.length : i)
}
B.lift = (id, mode) => {
	const c = B.card(id)
	if (!c) return
	const from = B.laneOf(c)
	$_board.drag = id
	$_board.mode = mode
	$_board.seen = +c.dataset.version
	$_board.from = from
	$_board.at = [...c.parentElement.children].indexOf(c)
	B.aim(from, $_board.at)
	if (mode === 'keys') B.say('Picked up “' + c.dataset.title + '”: ' + B.place(from, $_board.at, $_board.n) + '. Arrows choose a place, Space drops it, Escape puts it back.')
}
B.follow = (x, y) => {
	const p = B.press, box = el.getBoundingClientRect()
	el.querySelector('.bd-ghost').style.translate = (x - box.left - p.dx) + 'px ' + (y - box.top - p.dy) + 'px'
}
B.cancel = (text) => {
	const c = B.card($_board.drag)
	$_board.drag = 0
	$_board.mode = ''
	B.aim(-1, 0)
	if (text && c) B.say('“' + c.dataset.title + '” stays in ' + B.lane(B.laneOf(c)).dataset.name + '.')
}
B.drop = () => {
	const c = B.card($_board.drag)
	const lane = $_board.lane, index = $_board.index
	if (!c || lane < 0 || (lane === $_board.from && index === $_board.at)) { B.cancel(true); return null }
	const move = {card: $_board.drag, lane: lane, index: index, seen: $_board.seen}
	$_board.pcard = $_board.drag
	$_board.drag = 0
	$_board.mode = ''
	$_board.problem = ''
	$_board.op = Date.now().toString(36) + Math.random().toString(36).slice(2, 7)
	B.say('Moving “' + c.dataset.title + '” to ' + B.lane(lane).dataset.name + '.')
	return move
}
B.walk = (c, key) => {
	let lane = B.laneOf(c)
	const i = [...c.parentElement.children].indexOf(c)
	let next = null
	if (key === 'ArrowUp') next = c.previousElementSibling
	if (key === 'ArrowDown') next = c.nextElementSibling
	if (key === 'ArrowLeft' || key === 'ArrowRight') {
		const step = key === 'ArrowLeft' ? -1 : 1
		for (lane += step; !next && lane >= 0 && B.lane(lane); lane += step) {
			const cards = B.lane(lane).querySelectorAll('[data-card]')
			next = cards[Math.min(i, cards.length - 1)] || null
		}
	}
	next?.querySelector('button')?.focus()
}`

// press starts a drag: the main button of a mouse or pen anywhere on a
// card, a finger on its grip (the rest of the card scrolls the page). The
// stage captures the pointer, so the drag goes on outside it. A drag
// starts once the pointer has moved 4 px; a finger's at once. It listens
// on the window: while a view transition runs (someone's move gliding),
// the cards take no presses. Chromium gives a press to what lies under the
// transition's scope, the board, or to the root element during a
// transition of the whole page, and the card under the pointer is found by
// its box instead.
const press = `if (evt.button !== 0 || $_board.op) return
if (evt.target !== document.documentElement && !el.contains(evt.target)) return
const x = evt.clientX, y = evt.clientY
const under = (e) => { const r = e.getBoundingClientRect(); return x >= r.left && x < r.right && y >= r.top && y < r.bottom }
const hit = evt.target.closest('[data-card]')
const c = hit ?? [...el.querySelectorAll('[data-card]')].find(under)
if (!c) return
const touch = evt.pointerType === 'touch'
if (touch && !(hit ? evt.target.closest('.bd-card__grip') : under(c.querySelector('.bd-card__grip')))) return
if ($_board.mode === 'keys') el.bd.cancel(false)
const r = c.getBoundingClientRect()
el.bd.press = {id: +c.dataset.card, pointer: evt.pointerId, x: x, y: y, dx: x - r.left, dy: y - r.top}
el.setPointerCapture(evt.pointerId)
const g = el.querySelector('.bd-ghost')
g.replaceChildren(c.querySelector('.bd-card').cloneNode(true))
g.style.inlineSize = r.width + 'px'
if (touch) { el.bd.lift(el.bd.press.id, 'pointer'); el.bd.follow(x, y) }`

// drag moves the ghost with the pointer and aims at the place under it.
const drag = `const p = el.bd.press
if (!p || evt.pointerId !== p.pointer) return
if ($_board.mode !== 'pointer') {
	if (Math.hypot(evt.clientX - p.x, evt.clientY - p.y) < 4) return
	el.bd.lift(p.id, 'pointer')
}
el.bd.follow(evt.clientX, evt.clientY)
el.bd.aimAt(evt.clientX, evt.clientY)`

// release drops the card where the drag aims: the move goes to the server
// with the card's version from when it was picked up.
const release = `const p = el.bd.press
if (!p || evt.pointerId !== p.pointer) return
el.bd.press = null
if ($_board.mode !== 'pointer') return
const move = el.bd.drop()
move && ` + post

// lost ends a drag the browser took over (a scroll, a second finger).
const lost = `const p = el.bd.press
if (!p || evt.pointerId !== p.pointer) return
el.bd.press = null
if ($_board.mode === 'pointer') el.bd.cancel(false)`

// keys: on a card, Space or Enter picks it up and the arrows go from card
// to card; picked up, the arrows choose a place, Space or Enter drops it
// there and Escape or Tab puts it back.
const keys = `const k = evt.key, arrow = k.startsWith('Arrow')
if ($_board.mode === 'pointer') {
	if (k === 'Escape') {
		evt.preventDefault()
		const p = el.bd.press
		el.bd.press = null
		if (p && el.hasPointerCapture(p.pointer)) el.releasePointerCapture(p.pointer)
		el.bd.cancel(false)
	}
	return
}
if ($_board.mode === 'keys') {
	if (arrow) {
		evt.preventDefault()
		const step = k === 'ArrowLeft' ? -1 : k === 'ArrowRight' ? 1 : 0
		const lane = Math.max(0, Math.min(2, $_board.lane + step))
		const index = step ? $_board.index : $_board.index + (k === 'ArrowUp' ? -1 : 1)
		el.bd.aim(lane, index)
		const at = lane === $_board.from && $_board.index === $_board.at
		el.bd.say(el.bd.place(lane, $_board.index, $_board.n) + (at ? ', where it is' : '') + '.')
	} else if (k === ' ' || k === 'Enter') {
		evt.preventDefault()
		const move = el.bd.drop()
		move && ` + post + `
	} else if (k === 'Escape' || k === 'Tab') {
		k === 'Escape' && evt.preventDefault()
		el.bd.cancel(true)
	}
	return
}
const c = evt.target.closest('[data-card]')
if (!c || evt.altKey || evt.ctrlKey || evt.metaKey) return
if (k === ' ' || k === 'Enter') {
	evt.preventDefault()
	$_board.op ? el.bd.say('One move at a time: the last one is on its way.') : el.bd.lift(+c.dataset.card, 'keys')
} else if (arrow) {
	evt.preventDefault()
	el.bd.walk(c, k)
}`

// leave puts a card picked up from the keyboard back when the focus goes
// elsewhere: a click outside the board, another window.
const leave = `$_board.mode === 'keys' && !(evt.relatedTarget && el.contains(evt.relatedTarget)) && el.bd.cancel(true)`

// post sends the move in hand: requestCancellation 'disabled', so that a
// second move to the same URL never cancels the first.
const post = `@post('/cmd/board/move', {payload: {tab: $_tab, op: $_board.op, card: move.card, lane: move.lane, index: move.index, seen: move.seen}, requestCancellation: 'disabled'})`

// settle ends the pending move once its outcome has come and the board
// shows the version that has it. A refused card shakes for a moment.
const settle = `const op = $_board.op
if (!op || $_board.ack !== op || $_board.v < $_board.ackv) return
const card = $_board.pcard
if (!$_board.ok) {
	$_board.nudge = card
	setTimeout(() => { if ($_board.nudge === card) $_board.nudge = 0 }, 900)
}
$_board.op = ''
$_board.pcard = 0
$_board.problem = ''
if (!$_board.drag) el.bd.aim(-1, 0)`

// fetched says so while the move's request is retried (the op id makes a
// retry safe), and follows the stage's requests (demo.Pending.Fetched).
var fetched = `evt.detail.el === el && $_board.op && evt.detail.type === 'retrying' && ($_board.problem = 'The server does not answer. Trying again.');
` + moving.Fetched()

// looks is the style of the card in hand (--bd-lift under the pointer,
// which leaves a hole behind its ghost; --bd-pick from the keyboard,
// which rises), of a card pending or refused, and of the place the card
// would go, set by id from the signals. It lives outside the regions the
// stream morphs, so no frame takes it away. It sets custom properties and
// the marker's anchor; board.css draws them. The marker sits above the
// card that would follow the moved one, below the lane's last card, or at
// the top of an empty lane, and stays hidden while the place is the
// card's own. One expression: data-text returns its last statement.
const looks = `($_board.drag ? '#board-card-' + $_board.drag + ($_board.mode === 'keys' ? '{--bd-pick:1}' : '{--bd-lift:1}') : '') +
($_board.pcard ? '#board-card-' + $_board.pcard + '{--bd-wait:1;--bd-busy:bd-busy}' : '') +
($_board.nudge ? '#board-card-' + $_board.nudge + '{--bd-nudge:bd-nudge}' : '') +
($_board.lane < 0 ? '' : '[data-lane="' + $_board.lane + '"]{--bd-target:1}' + (
	$_board.drag && $_board.lane === $_board.from && $_board.index === $_board.at ? ''
	: $_board.n === 0 ? '[data-lane="' + $_board.lane + '"] .bd-lane__cards{anchor-name:--bd-slot}.bd-marker{--bd-on:1;--bd-edge:0}'
	: '[data-lane="' + $_board.lane + '"] .bd-slot:nth-' + ($_board.index < $_board.n ? 'child(' + ($_board.index + 1) : 'last-child(1') +
		' of :not(#board-card-' + ($_board.drag || $_board.pcard) + ')){anchor-name:--bd-slot}.bd-marker{--bd-on:1;--bd-edge:' + ($_board.index < $_board.n ? 0 : 1) + '}'))`

// hello tells the server this tab is on the page, for the presence row:
// once when the page loads, and again whenever the page's stream opens
// (after a deploy). It retries a refusal by the rate limit.
const hello = `@post('/cmd/board/hello', {payload: {tab: $_tab}, retry: 'error', retryMaxCount: 5})`

const helloAgain = `evt.detail.el.id === 'stream' && evt.detail.type === 'started' && ` + hello
