package canvas

import "fmt"

// The Datastar expressions of the canvas. Its signals live under
// $_canvas, in the browser: an action sends only its payload.

// signals are the tab's: the brush's colour, the palette in hex (false
// until the page has read it from the site's tokens), and what went wrong
// with the last action.
func signals(color int) string {
	return fmt.Sprintf(`{_canvas: {color: %d, palette: false, problem: ''}}`, color)
}

// start runs once on the stage. sb-pixel-board paints with sixteen hex
// colours; the page reads them from the site's tokens (--canvas-c0 to
// -c15 in canvas.css, each with a light and a dark value), through a
// probe that resolves a token and a 1 by 1 canvas that turns any CSS
// colour into sRGB. It reads them again when the theme changes. Then it
// tells the server the tab's brush, which adds the tab to the painters.
const start = `
const probe = el.querySelector('.cv__probe');
const pen = document.createElement('canvas').getContext('2d', {willReadFrequently: true});
const read = () => {
	$_canvas.palette = JSON.stringify(Array.from({length: 16}, (_, i) => {
		probe.style.color = 'var(--canvas-c' + i + ')';
		pen.fillStyle = getComputedStyle(probe).color;
		pen.fillRect(0, 0, 1, 1);
		const [r, g, b] = pen.getImageData(0, 0, 1, 1).data;
		return '#' + ((1 << 24) | (r << 16) | (g << 8) | b).toString(16).slice(1);
	}));
};
read();
new MutationObserver(read).observe(document.documentElement, {attributeFilter: ['data-lith-theme']});
matchMedia('(prefers-color-scheme: dark)').addEventListener('change', read);
@post('/cmd/canvas/pick', {payload: {tab: $_tab, color: $_canvas.color}})`

// paint sends a stroke's batch. A stroke sends one every 80 ms, and
// Datastar would cancel a request still under way when the next one goes
// to the same URL: the pixels of a slow request would be lost.
const paint = `@post('/cmd/canvas/paint', {payload: {tab: $_tab, color: evt.detail.color, cells: evt.detail.cells}, requestCancellation: 'disabled'})`

// refused says why pixels stay faded when their batch failed: over the
// rate limit, or the connection gone. sb-pixel-board takes them back
// after three seconds, since no frame confirms them.
const refused = `evt.detail.el === el && ['error', 'retries-failed'].includes(evt.detail.type) && ($_canvas.problem = 'Some pixels did not reach the canvas: each painter gets 20 a second, and the connection has to hold. They fade back; paint them again in a moment.')`

// choose makes the swatch the brush, at once.
const choose = `$_canvas.color = +evt.target.value; $_canvas.problem = ''`

// pick tells the server the brush a moment after the last change, so that
// arrowing through the swatches sends one pick.
const pick = `@post('/cmd/canvas/pick', {payload: {tab: $_tab, color: $_canvas.color}})`

// pickFailed: the brush changed on this page, but the list of painters
// still shows the old colour until the next paint.
const pickFailed = `evt.detail.el === el && ['error', 'retries-failed'].includes(evt.detail.type) && ($_canvas.problem = 'Your new colour did not reach the server; the painters see it with your next stroke.')`
