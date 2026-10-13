package guestbook

import "fmt"

// The Datastar expressions of the demo. Its signals live under
// $_guestbook, local to the browser: an action sends only its payload.

// signals are the demo's own, declared on its stage.
const signals = `{_guestbook: {name: '', message: '', op: '', ack: '', accepted: false, problem: ''}}`

// sign sends the form with a new op id unless one is pending. The tab's
// stream brings the op's outcome back as $_guestbook.ack.
const sign = `$_guestbook.op === '' && $_guestbook.name.trim() && $_guestbook.message.trim() && (
	$_guestbook.problem = '',
	$_guestbook.op = Date.now().toString(36) + Math.random().toString(36).slice(2, 7),
	@post('/cmd/guestbook/sign', {payload: {tab: $_tab, op: $_guestbook.op, name: $_guestbook.name, message: $_guestbook.message}})
)`

// pending is true from the click until the stream brings the outcome.
const pending = `$_guestbook.op !== ''`

// busy is pending for aria-busy: data-attr writes true as an empty value,
// which aria-busy reads as false.
const busy = `$_guestbook.op !== '' ? 'true' : false`

// settle ends the pending state when the outcome arrives, and empties the
// message once it was accepted.
const settle = `$_guestbook.op !== '' && $_guestbook.ack === $_guestbook.op && ($_guestbook.accepted && ($_guestbook.message = ''), $_guestbook.op = '')`

// failed ends the pending state when the action itself fails (refused by a
// rate limit, the network gone), since no outcome will come for it.
const failed = `evt.detail.el === el && ['error', 'retries-failed'].includes(evt.detail.type) && ($_guestbook.op = '', $_guestbook.problem = 'That did not go through. Wait a moment and try again.')`

func maxLen(n int) string { return fmt.Sprint(n) }
