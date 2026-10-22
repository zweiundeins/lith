package writerrace

import (
	"strconv"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// The Datastar expressions of the demo. Its signals live under
// $_writerRace, local to the browser: an action sends only its payload.

// signals are the demo's own, declared on its stage: the choices, the op
// of the start in flight, and what the tab's view patches back (ack, the
// tab's race, whether that race still waits or runs), with what
// demo.Pending keeps.
const signals = `{_writerRace: {producers: '200', sync: 'NORMAL', op: '', ack: '', mine: 0, active: false, synced: false, inflight: 0, recheck: false, problem: ''}}`

// op is the start's pending op: it ends on its ack (settle), a request that
// never got through (fetched), or a stream that connects again without its
// ack (lost), never on a clock.
var op = demo.NewPending("_writerRace")

// start sends the choices with a new op id unless one is pending. The
// tab's stream brings the op's outcome back as $_writerRace.ack.
const start = `$_writerRace.op === '' && !$_writerRace.active && (
	$_writerRace.problem = '',
	$_writerRace.op = Date.now().toString(36) + Math.random().toString(36).slice(2, 7),
	@post('/cmd/writer-race/start', {payload: {tab: $_tab, op: $_writerRace.op, producers: Number($_writerRace.producers), sync: $_writerRace.sync}, requestCancellation: 'disabled'})
)`

// busy is aria-busy from the click until the stream brings the outcome:
// data-attr writes true as an empty value, which aria-busy reads as
// false.
const busy = `$_writerRace.op !== '' ? 'true' : false`

// settle ends the pending state when the outcome arrives.
const settle = `$_writerRace.op !== '' && $_writerRace.ack === $_writerRace.op && ($_writerRace.op = '')`

// fetched follows the start's requests (demo.Pending.Fetched).
var fetched = op.Fetched()

// mine marks the tab's own race in the shared views.
func mine(id int64) string { return "$_writerRace.mine === " + strconv.FormatInt(id, 10) }
