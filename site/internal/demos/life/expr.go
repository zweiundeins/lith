package life

import (
	"fmt"
	"strings"
)

// The Datastar expressions of the demo. Its signals live under $_life, in
// the browser only: an action sends just its payload.
//
//	tool, turn     the tool (draw or a pattern's name, a glider at first), and the pattern's quarter turns,
//	               which only grow, so that the outline always turns clockwise
//	x, y           the cell under the pointer or the keyboard's cursor; -1: none
//	drawing, alive a stroke is under way, and whether it draws or clears
//	stroke, last   the stroke's points not sent yet ("12,13"), and the last one sent
//	draft, pend    the stroke's cells as an SVG path: not sent yet, and sent but not confirmed
//	px, py, ptool, pturn  the pattern dropped last, while it is not confirmed
//	op, ack        the newest action's op id, and the newest the tab's view confirmed
//	colour, pick   the colour the server says is ours, and the one we asked for
//	resetting      a reset is on its way
//	problem        why the last action did not go through
func signals(colour int) string {
	return fmt.Sprintf(`{_life: {tool: 'glider', turn: 0, x: -1, y: -1, drawing: false, alive: true, stroke: '', last: -1, sent: 0, draft: '', pend: '', px: -1, py: -1, ptool: '', pturn: 0, op: '', ack: '', colour: %d, pick: %d, resetting: false, problem: ''}}`, colour, colour)
}

// expr joins the parts of an expression, which Datastar runs as one.
func expr(parts ...string) string { return strings.Join(parts, ", ") }

// transposed is true on a field taller than wide: CSS stands the board on
// its side there, so a column on the screen is a row of the board.
const transposed = `el.clientHeight > el.clientWidth`

// point sets $_life.x and $_life.y to the cell under the pointer.
const point = `((r, t) => (
	$_life.x = Math.min(79, Math.max(0, Math.floor((t ? evt.clientY - r.top : evt.clientX - r.left) / (t ? r.height : r.width) * 80))),
	$_life.y = Math.min(49, Math.max(0, Math.floor((t ? evt.clientX - r.left : evt.clientY - r.top) / (t ? r.width : r.height) * 50)))
))(el.getBoundingClientRect(), ` + transposed + `)`

// newOp makes the op id of an action.
const newOp = `$_life.op = Date.now().toString(36) + Math.random().toString(36).slice(2, 7)`

// square is the SVG path of the cell at x, y: a pending mark.
const square = `'M' + $_life.x + ' ' + $_life.y + 'h1v1h-1z'`

// cell is the index of the cell at x, y.
const cell = `($_life.y * 80 + $_life.x)`

// flush sends the stroke's points not sent yet, joined by a line to the
// last one sent. Every batch must arrive: requestCancellation is off.
const flush = `$_life.stroke !== '' && (
	$_life.problem = '',
	` + newOp + `,
	$_life.pend += $_life.draft,
	$_life.draft = '',
	@post('/cmd/life/draw', {payload: {tab: $_tab, op: $_life.op, cells: ($_life.last >= 0 ? $_life.last + ',' + $_life.stroke : $_life.stroke).split(',').map(Number), alive: $_life.alive}, requestCancellation: 'disabled'}),
	$_life.last = Number($_life.stroke.split(',').pop()),
	$_life.stroke = '',
	$_life.sent = Date.now()
)`

// begin starts a stroke where the pointer went down: it clears where it
// starts on a live cell, and draws where it starts on a dead one.
var begin = `$_life.tool === 'draw' && evt.isPrimary && evt.button === 0 && (` + expr(
	point,
	`el.setPointerCapture(evt.pointerId)`,
	`$_life.drawing = true`,
	`$_life.alive = document.getElementById('life-board').children[`+cell+`].className === '_'`,
	`$_life.last = -1`,
	`$_life.stroke = String(`+cell+`)`,
	`$_life.draft = `+square,
	`$_life.sent = Date.now()`,
) + `)`

// move follows the pointer, adds each new cell to the stroke, and sends
// the stroke every 100 ms.
var move = expr(
	point,
	`$_life.drawing && ((c, s) => s.includes(',' + c + ',') || ($_life.stroke = $_life.stroke ? $_life.stroke + ',' + c : String(c), $_life.draft += `+square+`))(`+cell+`, ',' + $_life.stroke + ',')`,
	`$_life.drawing && ($_life.stroke.split(',').length >= 48 || Date.now() - $_life.sent > 100) && (`+flush+`)`,
)

// end sends the rest of the stroke.
const end = `$_life.drawing && ((` + flush + `), $_life.drawing = false)`

// leave hides the cursor when the pointer leaves the board.
const leave = `$_life.drawing || ($_life.x = -1)`

// drop sends the pattern at x, y, and shows it pending there.
const drop = `(
	$_life.problem = '',
	` + newOp + `,
	$_life.px = $_life.x, $_life.py = $_life.y, $_life.ptool = $_life.tool, $_life.pturn = $_life.turn,
	@post('/cmd/life/drop', {payload: {tab: $_tab, op: $_life.op, pattern: $_life.tool, cell: $_life.y * 80 + $_life.x, turn: $_life.turn % 4}, requestCancellation: 'disabled'})
)`

// click drops a pattern where the board was clicked.
var click = `$_life.tool !== 'draw' && (` + expr(point, drop) + `)`

// toggle flips the cell at x, y: the keyboard's draw.
var toggle = `(` + expr(
	`$_life.problem = ''`,
	newOp,
	`$_life.alive = document.getElementById('life-board').children[`+cell+`].className === '_'`,
	`$_life.pend += `+square,
	`@post('/cmd/life/draw', {payload: {tab: $_tab, op: $_life.op, cells: [`+cell+`], alive: $_life.alive}, requestCancellation: 'disabled'})`,
) + `)`

// key moves the keyboard's cursor with the arrows (the board wraps
// around), acts with Enter or Space, turns the pattern with R and hides
// the cursor with Escape.
var key = `((k, t) => k.startsWith('Arrow') ? (
	evt.preventDefault(),
	$_life.x < 0 && ($_life.x = 40, $_life.y = 25),
	((a, b) => ($_life.x = ($_life.x + (t ? b : a) + 80) % 80, $_life.y = ($_life.y + (t ? a : b) + 50) % 50))(
		k === 'ArrowRight' ? 1 : k === 'ArrowLeft' ? -1 : 0,
		k === 'ArrowDown' ? 1 : k === 'ArrowUp' ? -1 : 0)
) : k === 'Enter' || k === ' ' ? (
	evt.preventDefault(),
	$_life.x < 0 && ($_life.x = 40, $_life.y = 25),
	$_life.tool === 'draw' ? ` + toggle + ` : ` + drop + `
) : k === 'r' || k === 'R' ? (` + turn + `) : k === 'Escape' && ($_life.x = -1))(evt.key, ` + transposed + `)`

// turn turns the pattern a quarter clockwise.
const turn = `$_life.turn = $_life.turn + 1`

// pick asks for a colour, which stays pending until the tab's view
// brings it back as $_life.colour.
func pick(c int) string {
	return fmt.Sprintf(`$_life.pick = %d, $_life.problem = '', @post('/cmd/life/colour', {payload: {tab: $_tab, colour: %d}})`, c, c)
}

// picked and picking are a swatch's aria-pressed and aria-busy.
func picked(c int) string { return fmt.Sprintf(`$_life.colour === %d ? 'true' : 'false'`, c) }
func picking(c int) string {
	return fmt.Sprintf(`$_life.pick === %d && $_life.colour !== %d ? 'true' : false`, c, c)
}

// useTool picks a tool, and toolPressed is its button's aria-pressed.
func useTool(name string) string     { return `$_life.tool = '` + name + `'` }
func toolPressed(name string) string { return `$_life.tool === '` + name + `' ? 'true' : 'false'` }

// reset asks to clear the board.
const reset = `!$_life.resetting && (
	$_life.problem = '',
	` + newOp + `,
	$_life.resetting = true,
	@post('/cmd/life/reset', {payload: {tab: $_tab, op: $_life.op}})
)`

// resetting is the reset button's aria-busy: data-attr writes true as an
// empty value, which aria-busy reads as false.
const resetting = `$_life.resetting ? 'true' : false`

// settle ends what is pending once the tab's view confirms the newest op
// (op ids grow with time, so a newer one confirms the older ones too).
const settle = `$_life.op !== '' && $_life.ack >= $_life.op && ($_life.op = '', $_life.pend = '', $_life.ptool = '', $_life.resetting = false)`

// failed ends what is pending when an action itself fails (a rate limit,
// the network gone), since no confirmation will come for it.
const failed = `el.contains(evt.detail.el) && ['error', 'retries-failed'].includes(evt.detail.type) && (
	$_life.op = '', $_life.pend = '', $_life.draft = '', $_life.ptool = '', $_life.resetting = false,
	$_life.pick = $_life.colour,
	$_life.problem = 'That did not go through. Wait a moment and try again.'
)`

// cursor places the pattern's outline under the pointer or the cursor.
const cursor = `'--x:' + $_life.x + ';--y:' + $_life.y + ';--turn:' + $_life.turn`

// stamp places the outline of the pattern dropped last, until it is
// confirmed.
const stamp = `'--x:' + $_life.px + ';--y:' + $_life.py + ';--turn:' + $_life.pturn`

// me is the stage's --me, the colour this tab draws in, for the outlines
// and the pending marks.
const me = `'--me: var(--life-' + ' abcdefgh'[$_life.colour] + ')'`
