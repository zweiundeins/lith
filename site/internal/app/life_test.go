package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/app"
	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demos/life"
)

// TestLifeStream follows the board on a tab's stream: the clock steps it
// while the stream is open, a reset, a colour and a glider reach the next
// frames, a burst of resets meets the rate limit, and the clock stops when
// the stream closes.
func TestLifeStream(t *testing.T) {
	a, base := apptest.Start(t)
	c := client(t)
	get(t, c, base+"/demos/life")
	ctx, closeStream := context.WithCancel(context.Background())
	defer closeStream()
	events := lifeListen(t, ctx, c, base+"/demos/life/stream?tab=tab1")
	first := next(t, events, `id="life-board"`)
	if n := strings.Count(first, "<p class="); n != life.Cells {
		t.Errorf("the first frame has %d cells", n)
	}
	waitFor(t, "the clock to step", func() bool { return lifeGeneration(t, a) >= 3 })

	if code := post(t, c, base+"/cmd/life/reset", `{"tab":"tab1","op":"op1"}`); code != http.StatusNoContent {
		t.Fatalf("reset: %d", code)
	}
	next(t, events, "The board is empty.")
	ack := next(t, events, `"ack":"op1"`)
	// The next colour after the one the session has.
	var mine int
	fmt.Sscanf(ack[strings.Index(ack, `"colour":`)+len(`"colour":`):], "%d", &mine)
	colour := strconv.Itoa(mine%life.Colours + 1)
	if code := post(t, c, base+"/cmd/life/colour", `{"tab":"tab1","colour":`+colour+`}`); code != http.StatusNoContent {
		t.Fatalf("colour: %d", code)
	}
	next(t, events, `"colour":`+colour+`}`)
	if code := post(t, c, base+"/cmd/life/drop", `{"tab":"tab1","op":"op2","pattern":"glider","cell":1000,"turn":0}`); code != http.StatusNoContent {
		t.Fatalf("drop: %d", code)
	}
	cell := "<p class=" + "_abcdefgh"[mine%life.Colours+1:mine%life.Colours+2] + ">"
	glider := next(t, events, cell)
	if n := strings.Count(glider, cell); n != 5 {
		t.Errorf("a glider of %d cells", n)
	}
	next(t, events, `"ack":"op2"`)
	if code := post(t, c, base+"/cmd/life/draw", `{"tab":"tab1","op":"op3","cells":[2000,2010],"alive":true}`); code != http.StatusNoContent {
		t.Fatalf("draw: %d", code)
	}
	next(t, events, `"ack":"op3"`)
	for _, bad := range []string{`{"tab":"tab1","op":"op4","cells":[],"alive":true}`, `{"tab":"bad tab","op":"op4","cells":[1],"alive":true}`} {
		if code := post(t, c, base+"/cmd/life/draw", bad); code != http.StatusBadRequest {
			t.Errorf("draw %s: %d", bad, code)
		}
	}
	codes := []int{}
	for i := range 3 {
		codes = append(codes, post(t, c, base+"/cmd/life/reset", `{"tab":"tab1","op":"r`+strconv.Itoa(i)+`"}`))
	}
	if codes[0] != http.StatusNoContent || codes[2] != http.StatusTooManyRequests {
		t.Errorf("a burst of resets: %v", codes)
	}
	next(t, events, "Someone reset the board")

	closeStream()
	waitFor(t, "the stream to close", func() bool { return a.Hub().Count(life.BoardKey) == 0 })
	time.Sleep(2 * life.Idle)
	g := lifeGeneration(t, a)
	time.Sleep(4 * life.Idle)
	if after := lifeGeneration(t, a); after != g {
		t.Errorf("with nobody watching, the board went from generation %d to %d", g, after)
	}
}

// TestLife opens the Game of Life in two browsers. Ada resets the board,
// picks teal and drops a glider with a click and another from the
// keyboard, and draws a stroke; Bob's board shows each in her colour, and
// only her tab hears that a second reset came too soon.
//
//	LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestLife
func TestLife(t *testing.T) {
	a, base := apptest.Start(t)
	b := browser.Start(t)
	ada := b.NewPage(t, 1280, 900)
	ada.Navigate(t, base+"/demos/life")
	bob := b.NewPage(t, 1280, 900)
	bob.Navigate(t, base+"/demos/life")
	for _, p := range []*browser.Page{ada, bob} {
		p.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)
	}
	waitFor(t, "two tabs on the board", func() bool { return a.Hub().Count(life.BoardKey) == 2 })
	bob.Wait(t, `document.querySelector('#life-stats dd').textContent !== '0'`, 5*time.Second)

	ada.Eval(t, `document.querySelector('.life__reset .btn').click()`, nil)
	bob.Wait(t, `document.querySelector('#life-status').textContent.includes('The board is empty.')`, 5*time.Second)
	ada.Wait(t, `document.querySelector('.life__reset .btn').getAttribute('aria-busy') !== 'true'`, 5*time.Second)

	ada.Eval(t, `document.querySelector('.life__swatch.e').click()`, nil)
	ada.Wait(t, `document.querySelector('.life__swatch.e').getAttribute('aria-pressed') === 'true'`, 5*time.Second)
	const teal = `document.querySelectorAll('#life-board > div > .e').length`

	ada.Eval(t, `document.querySelector('.life__tool:nth-child(2)').click()`, nil) // the glider
	lifeClick(t, ada, 20, 15)
	bob.Wait(t, teal+` === 5`, 5*time.Second)
	ada.Wait(t, `!document.querySelector('.life__cursor--stamp').classList.contains('is-on')`, 5*time.Second)

	ada.Eval(t, `document.querySelector('.life__field').focus()`, nil)
	ada.Press(t, "Escape") // the cursor starts again from the middle, away from the first glider
	for range 3 {
		ada.Call(t, "Input.dispatchKeyEvent", map[string]any{"type": "rawKeyDown", "key": "ArrowRight", "code": "ArrowRight", "windowsVirtualKeyCode": 39})
		ada.Call(t, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": "ArrowRight", "code": "ArrowRight", "windowsVirtualKeyCode": 39})
	}
	ada.Press(t, "Enter")
	bob.Wait(t, teal+` === 10`, 5*time.Second)

	ada.Eval(t, `document.querySelector('.life__tool:nth-child(1)').click()`, nil) // draw
	lifeDrag(t, ada, 10, 40, 30, 40)
	bob.Wait(t, teal+` > 10`, 5*time.Second)
	ada.Wait(t, `document.querySelector('.life__marks path').getAttribute('d') === ''`, 5*time.Second)

	ada.Eval(t, `document.querySelector('.life__reset .btn').click()`, nil)
	ada.Wait(t, `document.querySelector('#life-outcome').textContent.includes('Someone reset the board')`, 5*time.Second)
	var outcome string
	bob.Eval(t, `document.querySelector('#life-outcome').textContent.trim()`, &outcome)
	if outcome != "" {
		t.Errorf("Bob hears %q about a reset he did not ask for", outcome)
	}
	for name, p := range map[string]*browser.Page{"Ada's board": ada, "Bob's board": bob} {
		if errs := p.Errors(); len(errs) > 0 {
			t.Errorf("%s: %v", name, errs)
		}
	}
}

// TestLifePhone drops a glider on a phone, where CSS stands the board on
// its side: a tap on the 10th column and 20th row of the screen lands on
// row 10, column 20 of the board, under the pattern's outline.
func TestLifePhone(t *testing.T) {
	a, base := apptest.Start(t)
	b := browser.Start(t)
	p := b.NewPage(t, 390, 844)
	p.Navigate(t, base+"/demos/life")
	p.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)
	if err := a.Writer().Do(context.Background(), writer.Func(func(tx *writer.Tx) error {
		_, err := tx.Exec(`UPDATE life_board SET cells = zeroblob(4000), quiet = ?`, time.Now().UnixMilli())
		return err
	})); err != nil {
		t.Fatal(err)
	}
	p.Wait(t, `document.querySelector('#life-status').textContent.includes('The board is empty.')`, 5*time.Second)
	var tall bool
	p.Eval(t, `(() => { const f = document.querySelector('.life__field'); f.scrollIntoView({block: 'center'}); return f.clientHeight > f.clientWidth })()`, &tall)
	if !tall {
		t.Fatal("the board does not stand on its side on a phone")
	}
	var r struct{ X, Y, W, H float64 }
	p.Eval(t, `(() => { const b = document.querySelector('.life__field').getBoundingClientRect(); return {x: b.left, y: b.top, w: b.width, h: b.height} })()`, &r)
	px, py := r.X+10.5*r.W/life.H, r.Y+20.5*r.H/life.W
	p.Call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": px, "y": py})
	var anchor struct{ X, Y float64 }
	p.Eval(t, `(() => { const s = document.querySelector('.life__cursor > .life__shape--side[data-shape="glider"]').getBoundingClientRect(); return {x: s.left + 1.5 * s.width / 3, y: s.top + 1.5 * s.height / 3} })()`, &anchor)
	if d := max(anchor.X-px, px-anchor.X, anchor.Y-py, py-anchor.Y); d > 1 {
		t.Errorf("the outline's anchor is at %.1f, %.1f, the pointer at %.1f, %.1f", anchor.X, anchor.Y, px, py)
	}
	for _, typ := range []string{"mousePressed", "mouseReleased"} {
		p.Call(t, "Input.dispatchMouseEvent", map[string]any{"type": typ, "x": px, "y": py, "button": "left", "buttons": 1, "clickCount": 1})
	}
	waitFor(t, "the glider on the board", func() bool {
		var cells []byte
		a.DB().R.QueryRow(`SELECT cells FROM life_board`).Scan(&cells)
		near := 0
		for y := 7; y <= 13; y++ {
			for x := 17; x <= 23; x++ {
				if cells[y*life.W+x] != 0 {
					near++
				}
			}
		}
		return near == 5 && life.Population(cells) == 5
	})
	if errs := p.Errors(); len(errs) > 0 {
		t.Errorf("the phone's page: %v", errs)
	}
}

// TestLifePageCost measures what a tab of the demo costs the browser: the
// CPU time of its main thread over 10 s of a fresh soup, in all and per
// frame the page applied. It only reports. A number above 1 slows the CPU down that many times, like
// DevTools' throttling.
//
//	LIFE_PAGE_COST=4 LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestLifePageCost -v
func TestLifePageCost(t *testing.T) {
	if os.Getenv("LIFE_PAGE_COST") == "" {
		t.Skip("set LIFE_PAGE_COST=1 to measure")
	}
	rate, _ := strconv.ParseFloat(os.Getenv("LIFE_PAGE_COST"), 64)
	a, base := apptest.Start(t)
	b := browser.Start(t)
	for range 2 {
		p := b.NewPage(t, 1440, 900)
		if rate > 1 {
			p.Call(t, "Emulation.setCPUThrottlingRate", map[string]any{"rate": rate})
		}
		p.Navigate(t, base+"/demos/life")
		p.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)
		p.Eval(t, lifeScrollToStage, nil)
		// A fresh soup: the board empty and quiet since long ago.
		if err := a.Writer().Do(context.Background(), writer.Func(func(tx *writer.Tx) error {
			_, err := tx.Exec(`UPDATE life_board SET cells = zeroblob(4000), quiet = 1`)
			return err
		})); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
		p.Call(t, "Performance.enable", map[string]any{"timeDomain": "threadTicks"})
		metrics := func() map[string]float64 {
			var r struct {
				Metrics []struct {
					Name  string
					Value float64
				}
			}
			json.Unmarshal(p.Call(t, "Performance.getMetrics", nil), &r)
			m := map[string]float64{}
			for _, x := range r.Metrics {
				m[x.Name] = x.Value
			}
			return m
		}
		// The generation counter changes once for each frame applied.
		p.Eval(t, `window.frames_ = 0; new MutationObserver(() => window.frames_++).observe(document.getElementById('life-stats'), {subtree: true, childList: true, characterData: true}); 0`, nil)
		before := metrics()
		time.Sleep(10 * time.Second)
		after := metrics()
		var frames float64
		p.Eval(t, `window.frames_`, &frames)
		d := func(k string) float64 { return after[k] - before[k] }
		ms := func(k string) float64 { return d(k) / max(frames, 1) * 1000 }
		t.Logf("main thread %.0f%% busy (script %.2f s, style %.2f s, layout %.2f s in 10 s); %.0f frames, each %.0f ms script, %.0f ms style, %.0f ms layout", d("ThreadTime")*10, d("ScriptDuration"), d("RecalcStyleDuration"), d("LayoutDuration"), frames, ms("ScriptDuration"), ms("RecalcStyleDuration"), ms("LayoutDuration"))
		p.Close(t)
	}
}

// lifeListen is listen with a context that closes the stream.
func lifeListen(t *testing.T, ctx context.Context, c *http.Client, url string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "identity")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream %s: %d", url, res.StatusCode)
	}
	events := make(chan string, 64)
	go func() {
		defer res.Body.Close()
		defer close(events)
		var event strings.Builder
		buf := make([]byte, 64<<10)
		var pending []byte
		for {
			n, err := res.Body.Read(buf)
			pending = append(pending, buf[:n]...)
			for {
				i := strings.Index(string(pending), "\n\n")
				if i < 0 {
					break
				}
				event.Write(pending[:i+1])
				pending = pending[i+2:]
				select {
				case events <- event.String():
				case <-ctx.Done():
					return
				}
				event.Reset()
			}
			if err != nil {
				return
			}
		}
	}()
	return events
}

// lifeGeneration reads the board's generation.
func lifeGeneration(t *testing.T, a *app.App) int64 {
	t.Helper()
	var g int64
	if err := a.DB().R.QueryRow(`SELECT generation FROM life_board`).Scan(&g); err != nil {
		t.Fatal(err)
	}
	return g
}

// lifeCellPoint is where the cell at x, y of a page's board is on the screen.
func lifeCellPoint(t *testing.T, p *browser.Page, x, y int) (float64, float64) {
	t.Helper()
	var r struct{ X, Y, W, H float64 }
	p.Eval(t, `(() => { const b = document.querySelector('.life__field').getBoundingClientRect(); return {x: b.left, y: b.top, w: b.width, h: b.height} })()`, &r)
	return r.X + (float64(x)+0.5)*r.W/life.W, r.Y + (float64(y)+0.5)*r.H/life.H
}

// lifeClick clicks the cell at x, y of the page's board.
func lifeClick(t *testing.T, p *browser.Page, x, y int) {
	t.Helper()
	p.Eval(t, `document.querySelector('.life__field').scrollIntoView({block: 'center'})`, nil)
	px, py := lifeCellPoint(t, p, x, y)
	for _, typ := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		p.Call(t, "Input.dispatchMouseEvent", map[string]any{"type": typ, "x": px, "y": py, "button": "left", "buttons": 1, "clickCount": 1})
	}
}

// lifeDrag draws with the mouse from one cell of the page's board to another.
func lifeDrag(t *testing.T, p *browser.Page, x0, y0, x1, y1 int) {
	t.Helper()
	p.Eval(t, `document.querySelector('.life__field').scrollIntoView({block: 'center'})`, nil)
	px, py := lifeCellPoint(t, p, x0, y0)
	p.Call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": px, "y": py})
	p.Call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": px, "y": py, "button": "left", "buttons": 1, "clickCount": 1})
	for i := 1; i <= 10; i++ {
		qx, qy := lifeCellPoint(t, p, x0+(x1-x0)*i/10, y0+(y1-y0)*i/10)
		p.Call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": qx, "y": qy, "button": "left", "buttons": 1})
		time.Sleep(30 * time.Millisecond)
	}
	qx, qy := lifeCellPoint(t, p, x1, y1)
	p.Call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "x": qx, "y": qy, "button": "left", "buttons": 0, "clickCount": 1})
}

// TestShotsLife writes screenshots of the Game of Life, with people's
// patterns on the soup: the page in both themes and the smooth style, on a
// phone, the board close up, and the outline of a pattern under the
// pointer.
//
//	LITH_SITE_SHOTS=/tmp/shots LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestShotsLife
func TestShotsLife(t *testing.T) {
	dir := os.Getenv("LITH_SITE_SHOTS")
	if dir == "" {
		t.Skip("set LITH_SITE_SHOTS to a directory for the screenshots")
	}
	a, base := apptest.Start(t)
	b := browser.Start(t)

	p := b.NewPage(t, 1440, 900)
	p.Navigate(t, base+"/demos/life")
	p.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)
	p.Wait(t, `Number(document.querySelector('#life-stats dd').textContent.replace(/,/g, '')) > 0`, 10*time.Second)
	lifeColourBoard(t, a)
	time.Sleep(4 * time.Second)

	shot := func(name string, page *browser.Page) {
		t.Helper()
		page.Eval(t, "document.fonts.ready.then(() => true)", nil)
		page.Screenshot(t, filepath.Join(dir, name+".png"), false)
		if errs := page.Errors(); len(errs) > 0 {
			t.Errorf("%s: %v", name, errs)
		}
	}
	p.Eval(t, lifeScrollToStage, nil)
	time.Sleep(300 * time.Millisecond)
	shot("life", p)
	p.ShotElement(t, ".demo-stage", filepath.Join(dir, "life-stage.png"), 1)
	p.ShotElement(t, ".life__field", filepath.Join(dir, "life-board.png"), 2)

	// A glider gun under the pointer, turned a quarter.
	p.Eval(t, `document.querySelector('.life__tool:nth-child(5)').click()`, nil)
	p.Eval(t, `document.querySelector('.life__turn').click()`, nil)
	lifeHover(t, p, 30, 20)
	time.Sleep(400 * time.Millisecond)
	p.ShotElement(t, ".life__field", filepath.Join(dir, "life-cursor.png"), 1.5)

	for name, cookie := range map[string]string{"life-light": "lith-theme=light", "life-smooth": "lith-style=smooth"} {
		q := b.NewPage(t, 1440, 900)
		q.Navigate(t, base+"/")
		q.Eval(t, "document.cookie = '"+cookie+"; Path=/'", nil)
		q.Navigate(t, base+"/demos/life")
		q.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)
		time.Sleep(1500 * time.Millisecond)
		q.Eval(t, lifeScrollToStage, nil)
		time.Sleep(300 * time.Millisecond)
		shot(name, q)
		q.ShotElement(t, ".life__field", filepath.Join(dir, name+"-board.png"), 2)
		q.Close(t)
	}

	m := b.NewPage(t, 390, 844)
	m.Navigate(t, base+"/demos/life")
	m.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)
	time.Sleep(1500 * time.Millisecond)
	m.Eval(t, lifeScrollToStage, nil)
	time.Sleep(300 * time.Millisecond)
	shot("life-mobile", m)

	// The whole page, "How it works" open; reduced motion turns off the
	// scroll-driven reveals, which a picture of the whole page would miss.
	full := b.NewPage(t, 1440, 900)
	full.Call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "reduce"}}})
	full.Navigate(t, base+"/demos/life")
	full.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)
	full.Eval(t, `document.querySelector('.howto').open = true`, nil)
	time.Sleep(time.Second)
	full.Eval(t, "document.fonts.ready.then(() => true)", nil)
	full.Screenshot(t, filepath.Join(dir, "life-page.png"), true)
	full.Close(t)

	// The demo's card among the others.
	cards := b.NewPage(t, 1440, 900)
	cards.Navigate(t, base+"/demos")
	cards.Eval(t, "document.fonts.ready.then(() => true)", nil)
	cards.ShotElement(t, `[data-demo-card="life"]`, filepath.Join(dir, "life-card.png"), 1.5)
	cards.Close(t)

	// After a reset: the empty board counts down to its next soup.
	p.Eval(t, `document.querySelector('.life__reset .btn').click()`, nil)
	p.Wait(t, `document.querySelector('#life-status').textContent.includes('The board is empty.')`, 5*time.Second)
	p.Eval(t, `document.querySelector('.life__tool:nth-child(2)').click()`, nil)
	lifeClick(t, p, 12, 10)
	time.Sleep(1200 * time.Millisecond)
	p.ShotElement(t, ".demo-stage", filepath.Join(dir, "life-empty.png"), 1)

	// A second reset too soon: only this tab hears why.
	p.Eval(t, `document.querySelector('.life__reset .btn').click()`, nil)
	p.Wait(t, `document.querySelector('#life-outcome').textContent.includes('Someone reset the board')`, 5*time.Second)
	time.Sleep(500 * time.Millisecond)
	p.ShotElement(t, ".life__foot", filepath.Join(dir, "life-refused.png"), 1.5)
}

// lifeScrollToStage scrolls the stage to just below the sticky header.
const lifeScrollToStage = `window.scrollTo(0, document.querySelector('.demo-stage').getBoundingClientRect().top + scrollY - document.querySelector('.site-header').offsetHeight - 12)`

// lifeColourBoard drops people's patterns on the board, in eight colours.
func lifeColourBoard(t *testing.T, a *app.App) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	drops := []struct {
		pattern    string
		x, y, turn int
	}{
		{"gun", 22, 8, 0}, {"pulsar", 64, 12, 0}, {"spaceship", 8, 30, 0}, {"glider", 40, 40, 1},
		{"glider", 70, 40, 2}, {"spaceship", 50, 24, 2}, {"pulsar", 16, 40, 0}, {"glider", 34, 22, 3},
	}
	for i, d := range drops {
		v := demo.Viewer{Session: fmt.Sprintf("shot-session-%d", i), Tab: fmt.Sprintf("shot-tab-%d", i)}
		if err := a.Writer().Do(ctx, life.Pick{Viewer: v, Colour: i + 1, At: now}); err != nil {
			t.Fatal(err)
		}
		if err := a.Writer().Do(ctx, life.Drop{Viewer: v, Op: fmt.Sprintf("op%d", i), Pattern: d.pattern, Cell: d.y*life.W + d.x, Turn: d.turn, At: now}); err != nil {
			t.Fatal(err)
		}
	}
}

// lifeHover moves the mouse over the cell at x, y of the page's board.
func lifeHover(t *testing.T, p *browser.Page, x, y int) {
	t.Helper()
	var r struct{ X, Y, W, H float64 }
	p.Eval(t, `(() => { const e = document.querySelector('.life__field'); e.scrollIntoView({block: 'center'}); const b = e.getBoundingClientRect(); return {x: b.left, y: b.top, w: b.width, h: b.height} })()`, &r)
	p.Call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": r.X + (float64(x)+0.5)*r.W/life.W, "y": r.Y + (float64(y)+0.5)*r.H/life.H})
}
