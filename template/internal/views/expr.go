// Package views renders the pages and the regions that streams patch. A
// shared region (Entries) holds nothing about the viewer: the same bytes go
// to every tab.
package views

import (
	"fmt"
	"strconv"
)

// signals are the page's, all local (_): an action sends only its payload.
func signals(tab string) string {
	return fmt.Sprintf(`{_tab: %q, _slow: %s, _name: '', _message: '', _op: '', _ack: '', _accepted: false, _problem: ''}`, tab, slowLink)
}

// slowLink is true on a slow link, measured once as the page starts. Where
// the browser has navigator.connection (Chromium): data saving, or a link
// it calls 2g or 3g. Elsewhere, from Navigation Timing: the document's
// first byte more than 300 ms after its request, or under 100 KB/s.
const slowLink = `((c, n) => c ? c.saveData || ['slow-2g', '2g', '3g'].includes(c.effectiveType) : !!n && n.transferSize > 0 && (n.responseStart - n.requestStart > 300 || n.transferSize < 100 * (n.responseEnd - n.responseStart)))(navigator.connection, performance.getEntriesByType('navigation')[0])`

// openStream opens the tab's stream on the element with id "stream". On a
// slow link, measured as the page starts, it sends Save-Data: on
// (edge.SlowLink). The URL stays the same, so a stream opened again aborts
// the old request. retry 'error'
// retries through network errors, 502s and 503s and never gives up, but
// leaves a stream the server ends closed: on a deploy the server ends it
// with sse.Reconnect, which runs this again.
const openStream = `@get('/stream?tab=' + $_tab, {headers: $_slow ? {'Save-Data': 'on'} : {}, retry: 'error', retryMaxCount: Infinity, retryMaxWait: 10000})`

// behind asks the server to pace the tab's stream each time newest-frame.js
// reports that the page fell behind it (newest-frame-behind); /pace counts
// the report for app_live_newest_frame_behind_total too. It runs on
// <body>, where the event bubbles to: a @post from the stream's element
// would abort its stream.
const behind = `@post('/pace?tab=' + $_tab, {requestCancellation: 'disabled'})`

// sign sends the form with a new op id unless one is pending. The tab's
// stream brings the op's outcome back as $_ack.
const sign = `$_op === '' && $_name.trim() && $_message.trim() && (
	$_problem = '',
	$_op = Date.now().toString(36) + Math.random().toString(36).slice(2, 7),
	@post('/cmd/sign', {payload: {tab: $_tab, op: $_op, name: $_name, message: $_message}})
)`

// pending is true from the click until the stream brings the outcome.
const pending = `$_op !== ''`

// busy is pending for aria-busy: data-attr writes true as an empty value,
// which aria-busy reads as false.
const busy = `$_op !== '' ? 'true' : false`

// settle ends the pending state when the outcome arrives, and empties the
// message once it was accepted.
const settle = `$_op !== '' && $_ack === $_op && ($_accepted && ($_message = ''), $_op = '')`

// failed ends the pending state when the action itself fails, since no
// outcome will come for it.
const failed = `evt.detail.el === el && ['error', 'retries-failed'].includes(evt.detail.type) && ($_op = '', $_problem = 'That did not go through. Try again in a moment.')`

func count(n int) string {
	switch n {
	case 0:
		return "No entries yet"
	case 1:
		return "1 entry"
	}
	return strconv.Itoa(n) + " entries"
}
