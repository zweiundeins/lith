package web

import (
	"fmt"
	"strconv"

	"github.com/zweiundeins/lith/site/internal/docs/example/board"
)

// The page's Datastar expressions. Its signals start with _, so they stay
// in the browser: an action sends only its payload.

// StreamID is the element whose data-init opens the tab's stream: on a
// deploy, the stream's last event (live.Spec.Reconnect) replaces it with
// a copy, and the copy opens the stream again on the next process.
const StreamID = "stream"

// signals are the page's: the tab's id, new for every document, the add
// form's title, the pending op and its ack, and the heartbeat's watch.
func signals(tab string, p board.Prefs) string {
	return fmt.Sprintf(`{_tab: %q, _title: '', _op: '', _ack: '', _refused: false, _problem: '', _compact: %t, _ping: 0, _beat: 0}`, tab, p.Compact)
}

// openStream opens the tab's stream. retry 'error' retries network errors,
// 502s and 503s without end, which carries the page through a restart,
// and leaves closed a stream the server ended.
func openStream(b int64) string {
	return "@get('/boards/" + strconv.FormatInt(b, 10) + "/stream?tab=' + $_tab, {retry: 'error', retryMaxCount: Infinity, retryMaxWait: 10000})"
}

// beat notes a sign of life: the hub patches $_ping every 20 s
// (live.Options.HeartbeatSignal), frames or not.
const beat = `$_ping, $_beat = Date.now()`

// watchdog opens the stream again when no heartbeat came for 50 s: a
// connection that went quiet without closing, as a laptop's after sleep.
func watchdog(b int64) string {
	return "Date.now() - $_beat > 50000 && ($_beat = Date.now(), " + openStream(b) + ")"
}

// act posts an action with a new op id, unless one is pending. The tab's
// stream brings the op's outcome back as $_ack.
func act(path, payload string) string {
	return `$_op === '' && ($_problem = '', $_op = Date.now().toString(36) + Math.random().toString(36).slice(2, 7),
	@post('` + path + `', {payload: {tab: $_tab, op: $_op, ` + payload + `}}))`
}

// add adds a card with the title the form holds.
func add(lane int64) string {
	return "$_title.trim() && " + act("/cmd/add", "lane: "+strconv.FormatInt(lane, 10)+", title: $_title")
}

// onBoard handles a click on a card's button: one with data-lane moves the
// card on, with the version the page shows (a card that moved meanwhile
// stays, and the tab hears why); the other opens the card's panel.
var onBoard = `evt.target.dataset.card && (evt.target.dataset.lane
	? ` + act("/cmd/move", "card: +evt.target.dataset.card, lane: +evt.target.dataset.lane, seen: +evt.target.dataset.seen") + `
	: ` + panel("+evt.target.dataset.card") + `)`

// pending is true from the click until the stream brings the outcome.
const pending = `$_op !== ''`

// busy is pending for aria-busy: data-attr writes true as an empty value,
// which aria-busy reads as false.
const busy = `$_op !== '' ? 'true' : false`

// settle ends the pending state when the op's outcome arrives, and empties
// the title once a card was added.
const settle = `$_op !== '' && $_ack === $_op && (!$_refused && ($_title = ''), $_op = '')`

// failed ends the pending state when the action's request fails (a rate
// limit, the network gone): no outcome will come for it.
const failed = `evt.detail.el === el && ['error', 'retries-failed'].includes(evt.detail.type) && ($_op = '', $_problem = 'That did not go through. Try again in a moment.')`

// panel opens a card's panel in this tab, or closes it with card 0.
func panel(card string) string {
	return "@post('/cmd/panel', {payload: {tab: $_tab, card: " + card + "}})"
}

// compact sets the session's preference, for every tab of the session.
const compact = `@post('/cmd/compact', {payload: {compact: !$_compact}})`

func id(n int64) string { return strconv.FormatInt(n, 10) }

// people says how many tabs show the board.
func people(n int) string {
	if n == 1 {
		return "1 tab on this board"
	}
	return strconv.Itoa(n) + " tabs on this board"
}

// firstLane is the lane the add form adds to.
func firstLane(b board.Board) int64 {
	if len(b.Lanes) == 0 {
		return 0
	}
	return b.Lanes[0].ID
}
