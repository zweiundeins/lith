package stream

// The Datastar expressions of the demo. Its signals live under $_stream,
// local to the browser: the reports fill them in, and an action sends only
// its payload.

// signals are the demo page's own, declared on its stage: the report's
// (PageReport), the tab's choices, and the pending change.
func signals(st TabState) string {
	return `{_stream: {n: 0, coding: '', tier: '', heap: '', events: '', wire: '', total: '', fe: '', fw: 0, fwt: '', fr: 0, ratio: '', fp: 0, fps: '0', beats: 0, ` +
		`streams: 0, reserved: '', limit: '', budget: -1, tiers: '', quiet: 0, ` +
		`size: '` + string(st.size()) + `', churn: '` + string(st.churn()) + `', op: '', want: '', ack: '', problem: ''}}`
}

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
	@post('/cmd/stream/payload', {payload: {tab: $_tab, op: $_stream.op, ` + field + `: '` + value + `'}})
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

// failed ends the pending state when the action itself fails (a rate
// limit, the network gone), since no outcome will come for it.
const failed = `el.contains(evt.detail.el) && ['error', 'retries-failed'].includes(evt.detail.type) && ($_stream.op = '', $_stream.want = '', $_stream.problem = 'That did not go through. Wait a moment and try again.')`

// push adds the last frame's bytes on the wire to the sparkline with every
// report, a repeat of the same number too.
const push = `$_stream.n > 0 && el.push && el.push($_stream.fw)`
