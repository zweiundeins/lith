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
	return fmt.Sprintf(`{_tab: %q, _name: '', _message: '', _op: '', _ack: '', _accepted: false, _problem: ''}`, tab)
}

// openStream opens the tab's stream on the element with id "stream".
// retry 'error' retries through network errors, 502s and 503s and never
// gives up, but leaves a stream the server ends closed: on a deploy the
// server ends it with sse.Reconnect, which runs this again.
const openStream = `@get('/stream?tab=' + $_tab, {retry: 'error', retryMaxCount: Infinity, retryMaxWait: 10000})`

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
