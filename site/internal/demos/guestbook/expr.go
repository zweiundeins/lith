package guestbook

import (
	"fmt"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// The Datastar expressions of the demo. Its signals live under
// $_guestbook, local to the browser: an action sends only its payload.

// signals are the demo's own, declared on its stage.
const signals = `{_guestbook: {name: '', message: '', op: '', ack: '', accepted: false, synced: false, inflight: 0, recheck: false, problem: ''}}`

// op is the form's pending op: it ends on its ack (settle), a request that
// never got through (fetched), or a stream that connects again without its
// ack (lost), never on a clock.
var op = demo.NewPending("_guestbook")

// sign sends the form with a new op id unless one is pending. The tab's
// stream brings the op's outcome back as $_guestbook.ack.
const sign = `$_guestbook.op === '' && $_guestbook.name.trim() && $_guestbook.message.trim() && (
	$_guestbook.problem = '',
	$_guestbook.op = Date.now().toString(36) + Math.random().toString(36).slice(2, 7),
	@post('/cmd/guestbook/sign', {payload: {tab: $_tab, op: $_guestbook.op, name: $_guestbook.name, message: $_guestbook.message}, requestCancellation: 'disabled'})
)`

// pending is true from the click until the stream brings the outcome.
const pending = `$_guestbook.op !== ''`

// busy is pending for aria-busy: data-attr writes true as an empty value,
// which aria-busy reads as false.
const busy = `$_guestbook.op !== '' ? 'true' : false`

// settle ends the pending state when the outcome arrives, and empties the
// message once it was accepted.
const settle = `$_guestbook.op !== '' && $_guestbook.ack === $_guestbook.op && ($_guestbook.accepted && ($_guestbook.message = ''), $_guestbook.op = '')`

// fetched follows the form's requests (demo.Pending.Fetched).
var fetched = op.Fetched()

func maxLen(n int) string { return fmt.Sprint(n) }
