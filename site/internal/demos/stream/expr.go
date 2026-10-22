package stream

import "github.com/zweiundeins/lith/site/internal/demo"

// The Datastar expressions of the demo. Its signals live under $_stream,
// local to the browser: the reports fill them in, and an action sends only
// its payload.

// signals are the demo page's own, declared on its stage: the report's
// (PageReport), the tab's choices, and the pending change, with what
// demo.Pending keeps.
func signals(st TabState) string {
	return `{_stream: {n: 0, coding: '', tier: '', heap: '', events: '', wire: '', total: '', fe: '', fw: 0, fwt: '', fr: 0, ratio: '', fp: 0, fps: '0', beats: 0, ` +
		`streams: 0, reserved: '', limit: '', budget: -1, tiers: '', quiet: 0, ` +
		`size: '` + string(st.size()) + `', churn: '` + string(st.churn()) + `', op: '', want: '', ack: '', synced: false, inflight: 0, recheck: false, problem: ''}}`
}

// op is the pending change: it ends on its ack (settle), a request that
// never got through (fetched), or a stream that connects again without its
// ack (lost), never on a clock. The buttons inside the controls send it.
var op = func() demo.Pending {
	p := demo.NewPending("_stream")
	p.Match = "el.contains(evt.detail.el)"
	p.End = "$_stream.op = '', $_stream.want = ''"
	return p
}()

// embedSignals are the landing page slot's (EmbedReport).
const embedSignals = `{_stream: {n: 0, coding: '', tier: '', total: '', fw: 0, fwt: '', quiet: 0}}`

// tick counts the seconds since the last report, which sets it to 0.
const tick = `$_stream.quiet = $_stream.quiet + 1`

// choose sends a change of the payload with a new op id, unless one is
// pending or the tab has that choice already. The tab's stream brings the
// op back as $_stream.ack, with the choice.
func choose(field string, value string) string {
	return `$_stream.op === '' && $_stream.` + field + ` !== '` + value + `' && (
	$_stream.problem = '',
	$_stream.op = Date.now().toString(36) + Math.random().toString(36).slice(2, 7),
	$_stream.want = '` + field + `:` + value + `',
	@post('/cmd/stream/payload', {payload: {tab: $_tab, op: $_stream.op, ` + field + `: '` + value + `'}, requestCancellation: 'disabled'})
)`
}

// pressed is the button of the tab's choice, as the server last said.
func pressed(field, value string) string {
	return `$_stream.` + field + ` === '` + value + `' ? 'true' : 'false'`
}

// busy marks the button whose change is on its way; aria-busy reads an
// empty value as false, so false removes it.
func busy(field, value string) string {
	return `$_stream.want === '` + field + `:` + value + `' ? 'true' : false`
}

// settle ends the pending state when the tab's stream brings the op back.
const settle = `$_stream.op !== '' && $_stream.ack === $_stream.op && ($_stream.op = '', $_stream.want = '')`

// fetched follows the controls' requests (demo.Pending.Fetched).
var fetched = op.Fetched()

// push adds the last frame's bytes on the wire to the sparkline with every
// report, a repeat of the same number too.
const push = `$_stream.n > 0 && el.push && el.push($_stream.fw)`
