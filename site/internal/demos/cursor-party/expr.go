package cursorparty

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/a-h/templ"
)

// The Datastar expressions of the party. Its signals live under
// $_cursorParty, local to the browser: a move sends only its payload.

// signals are the page's own: the count of its moves, which orders them.
const signals = `{_cursorParty: {seq: 0}}`

// move sends where the pointer is, as fractions of the party's floor (el),
// when it is over the floor. Moves go out side by side: none cancels the
// one before (requestCancellation), none is retried, and the server keeps
// the one with the highest number.
const move = `const r = el.getBoundingClientRect(), x = (evt.clientX - r.left) / r.width, y = (evt.clientY - r.top) / r.height;
evt.isPrimary && x >= 0 && x <= 1 && y >= 0 && y <= 1 && @post('/cmd/cursor-party/move', {
	payload: {tab: $_tab, x: Math.round(x * 1e4) / 1e4, y: Math.round(y * 1e4) / 1e4, seq: ++$_cursorParty.seq},
	requestCancellation: 'disabled', retry: 'never', retryMaxCount: 0,
}).catch(() => {})`

// floor are the floor's listeners. The pointer is followed on the window,
// so that the hero's links and buttons, which sit above the floor, take
// nothing away: a move every Throttle at most (Datastar's throttle), and
// one more where the pointer comes to rest (its debounce), since the
// throttle drops the moves between two of its own. A tap or a click
// sends its place too.
func floor() templ.Attributes {
	return templ.Attributes{
		"data-on:pointermove__window__throttle." + ms(Throttle): move,
		"data-on:pointermove__window__debounce." + ms(Rest):     move,
		"data-on:pointerdown": move,
	}
}

func ms(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
}

// countStyle hands the counter's numbers to CSS, which rolls them from
// one value to the next (@property --cursor-party-here and -rate).
func countStyle(here, rate int) templ.SafeCSS {
	return templ.SafeCSS(fmt.Sprintf("--cursor-party-here:%d;--cursor-party-rate:%d", here, rate))
}

// rate is the counter's moves a second: those of the last whole second.
func rate(h []int) int { return h[len(h)-1] }

// spark is the sparkline's data, a JSON array.
func spark(h []int) string {
	b, _ := json.Marshal(h)
	return string(b)
}

// sparkTop is the top of the sparkline's scale: its highest point, and at
// least the 15 moves a second of one pointer, so a quiet party stays low.
func sparkTop(h []int) string { return strconv.Itoa(max(15, slices.Max(h))) }

// countText is the counter for those who do not see it.
func countText(here, rate int) string {
	people := "people"
	if here == 1 {
		people = "person"
	}
	return fmt.Sprintf("%d %s here, %d moves a second", here, people, rate)
}
