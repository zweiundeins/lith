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

// measure finds the anchors' boxes on this page (Anchors), relative to
// the floor (el): those the page lacks or hides stand at their homes. It
// hands them to CSS (--cp-<name>-x, -y, -w, -h on the floor, which the
// stream never patches) and keeps them for move, and measures again when
// the floor or an anchor changes size.
const measure = `const names = NAMES, homes = HOMES;
const find = (n) => document.querySelector('[data-cp-anchor="' + n + '"]');
const run = () => {
	const f = el.getBoundingClientRect(), at = {};
	for (const n of names) {
		const a = find(n), r = a && a.getBoundingClientRect(), h = homes[n];
		const b = r && r.width && r.height ? [r.left - f.left, r.top - f.top, r.width, r.height] : [h[0] * f.width, h[1] * f.height, h[2] * f.width, h[3] * f.height];
		at[n] = b;
		['x', 'y', 'w', 'h'].forEach((k, i) => el.style.setProperty('--cp-' + n + '-' + k, b[i] + 'px'));
	}
	el.cpAnchors = at;
};
run();
const ro = new ResizeObserver(run);
ro.observe(el);
for (const n of names) { const a = find(n); a && ro.observe(a) }`

// move sends where the pointer is, when it is over the floor (el): the
// anchor nearest to it, and its place as fractions of that anchor's box.
// Moves go out side by side: none cancels the one before
// (requestCancellation), none is retried, and the server keeps the one
// with the highest number.
const move = `const f = el.getBoundingClientRect(), px = evt.clientX - f.left, py = evt.clientY - f.top, at = el.cpAnchors;
if (evt.isPrimary && at && px >= 0 && py >= 0 && px <= f.width && py <= f.height) {
	let best = '', d = Infinity;
	for (const n in at) {
		const b = at[n], dx = Math.max(b[0] - px, 0, px - b[0] - b[2]), dy = Math.max(b[1] - py, 0, py - b[1] - b[3]);
		if (dx * dx + dy * dy < d) { d = dx * dx + dy * dy; best = n }
	}
	const b = at[best], r = (v) => Math.max(-20, Math.min(20, Math.round(v * 1e4) / 1e4));
	@post('/cmd/cursor-party/move', {
		payload: {tab: $_tab, a: best, x: r((px - b[0]) / b[2]), y: r((py - b[1]) / b[3]), seq: ++$_cursorParty.seq},
		requestCancellation: 'disabled', retry: 'never', retryMaxCount: 0,
	}).catch(() => {})
}`

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
		"data-init":           anchorHomes(),
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
