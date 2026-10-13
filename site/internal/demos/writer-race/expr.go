package writerrace

import "strconv"

// The Datastar expressions of the demo. Its signals live under
// $_writerRace, local to the browser: an action sends only its payload.

// signals are the demo's own, declared on its stage: the choices, the op
// of the start in flight, and what the tab's view patches back (ack, the
// tab's race, whether that race still waits or runs).
const signals = `{_writerRace: {producers: '200', sync: 'NORMAL', op: '', ack: '', mine: 0, active: false, problem: ''}}`

// start sends the choices with a new op id unless one is pending. The
// tab's stream brings the op's outcome back as $_writerRace.ack.
const start = `$_writerRace.op === '' && !$_writerRace.active && (
	$_writerRace.problem = '',
	$_writerRace.op = Date.now().toString(36) + Math.random().toString(36).slice(2, 7),
	@post('/cmd/writer-race/start', {payload: {tab: $_tab, op: $_writerRace.op, producers: Number($_writerRace.producers), sync: $_writerRace.sync}})
)`

// busy is aria-busy from the click until the stream brings the outcome:
// data-attr writes true as an empty value, which aria-busy reads as
// false.
const busy = `$_writerRace.op !== '' ? 'true' : false`

// settle ends the pending state when the outcome arrives.
const settle = `$_writerRace.op !== '' && $_writerRace.ack === $_writerRace.op && ($_writerRace.op = '')`

// failed ends the pending state when the action itself fails (refused by
// a rate limit, the network gone), since no outcome will come for it.
const failed = `evt.detail.el === el && ['error', 'retries-failed'].includes(evt.detail.type) && ($_writerRace.op = '', $_writerRace.problem = 'That did not go through. Wait a moment and try again.')`

// mine marks the tab's own race in the shared views.
func mine(id int64) string { return "$_writerRace.mine === " + strconv.FormatInt(id, 10) }
