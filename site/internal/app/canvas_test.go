package app_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/app"
	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demos/canvas"
)

// canvasScene is a picture for the canvas's screenshots, in palette
// indices: a monolith under the stars on a hill, its seam aglow, and what
// a few painters left around it.
func canvasScene() [canvas.Cells]int {
	var g [canvas.Cells]int
	put := func(x, y, c int) {
		if x >= 0 && y >= 0 && x < canvas.Size && y < canvas.Size {
			g[y*canvas.Size+x] = c
		}
	}
	rect := func(x0, y0, x1, y1, c int) {
		for y := y0; y <= y1; y++ {
			for x := x0; x <= x1; x++ {
				put(x, y, c)
			}
		}
	}
	// The hill, its grass, a few flowers.
	for x := range canvas.Size {
		top := 50 + (x-32)*(x-32)/90
		rect(x, top, x, 63, 5)
		put(x, top, 13)
		if x%3 == 0 {
			put(x, top-1, 12)
		}
	}
	for _, f := range [][3]int{{6, 52, 14}, {11, 53, 15}, {48, 52, 14}, {55, 54, 11}, {59, 56, 14}} {
		put(f[0], f[1], f[2])
		put(f[0], f[1]+1, 13)
	}
	// The monolith: lit on its left, in shadow on its right, a top face,
	// its seam wandering down with the glow beside it.
	rect(26, 14, 37, 50, 4)
	rect(26, 14, 26, 50, 3)
	rect(34, 14, 37, 50, 5)
	rect(26, 11, 37, 13, 2)
	rect(34, 11, 37, 13, 3)
	x := 30
	for y := 14; y <= 50; y++ {
		if y%9 == 4 && x < 32 {
			x++
			put(x-1, y, 7)
		} else if y%13 == 0 && x > 28 {
			x--
			put(x+1, y, 7)
		}
		put(x, y, 6)
		if y%3 == 0 {
			put(x+1, y, 8)
		}
		if y%4 == 1 {
			put(x-1, y, 9)
		}
	}
	for y := 47; y <= 50; y++ {
		put(x-1, y, 7)
		put(x+1, y, 7)
	}
	// The moon and the stars.
	for y := -4; y <= 4; y++ {
		for x := -4; x <= 4; x++ {
			if x*x+y*y <= 17 {
				put(10+x, 10+y, 2)
			}
		}
	}
	put(8, 9, 3)
	put(11, 12, 3)
	put(12, 8, 1)
	for _, s := range [][3]int{{20, 4, 6}, {44, 7, 11}, {52, 3, 6}, {58, 14, 7}, {47, 22, 6}, {17, 22, 11}, {4, 28, 6}, {56, 30, 11}, {41, 2, 6}} {
		put(s[0], s[1], s[2])
	}
	// A painter's sun and a tree.
	for _, p := range [][2]int{{0, 0}, {-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
		put(54+p[0], 10+p[1], 10)
	}
	rect(9, 40, 9, 48, 9)
	rect(6, 35, 12, 39, 13)
	rect(7, 33, 11, 34, 12)
	rect(8, 41, 10, 41, 13)
	// "hi" in the corner, from someone.
	for _, p := range [][2]int{{44, 40}, {44, 41}, {44, 42}, {44, 43}, {44, 44}, {45, 42}, {46, 42}, {46, 43}, {46, 44}, {48, 40}, {48, 42}, {48, 43}, {48, 44}} {
		put(p[0], p[1], 14)
	}
	return g
}

// paintCanvasScene paints the scene through the writer, a colour at a
// time, as a few painters would.
func paintCanvasScene(t *testing.T, a *app.App) {
	t.Helper()
	g := canvasScene()
	byColor := map[int][]int{}
	for i, c := range g {
		if c != 0 {
			byColor[c] = append(byColor[c], i)
		}
	}
	at := time.Now()
	for c, cells := range byColor {
		for start := 0; start < len(cells); start += canvas.MaxPaint {
			end := min(start+canvas.MaxPaint, len(cells))
			v := demo.Viewer{Session: "scene", Tab: "scene" + string(rune('a'+c))}
			if err := a.Writer().Do(context.Background(), canvas.Paint{Viewer: v, Color: c, Cells: cells[start:end], At: at}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// canvasPen draws on a page's canvas with the mouse, by cells: down moves
// there and presses, to drags with the button held, up releases.
type canvasPen struct {
	t       *testing.T
	p       *browser.Page
	x, y, w float64
}

func canvasPenOn(t *testing.T, p *browser.Page) *canvasPen {
	t.Helper()
	var r struct{ X, Y, W float64 }
	p.Eval(t, `(() => { const b = document.querySelector('#canvas-board').shadowRoot.querySelector('canvas').getBoundingClientRect(); return {x: b.left, y: b.top, w: b.width} })()`, &r)
	return &canvasPen{t: t, p: p, x: r.X, y: r.Y, w: r.W}
}

func (pn *canvasPen) mouse(typ string, x, y int, pressed bool) {
	button, buttons := "none", 0
	if pressed || typ == "mouseReleased" {
		button = "left"
	}
	if pressed {
		buttons = 1
	}
	cell := pn.w / 64
	pn.p.Call(pn.t, "Input.dispatchMouseEvent", map[string]any{
		"type": typ, "x": pn.x + (float64(x)+0.5)*cell, "y": pn.y + (float64(y)+0.5)*cell,
		"button": button, "buttons": buttons, "clickCount": 1,
	})
}

func (pn *canvasPen) down(x, y int) {
	pn.mouse("mouseMoved", x, y, false)
	pn.mouse("mousePressed", x, y, true)
}

func (pn *canvasPen) to(x, y int) {
	pn.mouse("mouseMoved", x, y, true)
	time.Sleep(10 * time.Millisecond)
}

func (pn *canvasPen) up(x, y int) { pn.mouse("mouseReleased", x, y, false) }

// TestShotsCanvas writes screenshots of the canvas with a picture on it
// and a few painters on the page: both themes, the smooth style, a phone.
//
//	LITH_SITE_SHOTS=/tmp/shots LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestShotsCanvas
func TestShotsCanvas(t *testing.T) {
	dir := os.Getenv("LITH_SITE_SHOTS")
	if dir == "" {
		t.Skip("set LITH_SITE_SHOTS to a directory for the screenshots")
	}
	a, base := apptest.Start(t)
	paintCanvasScene(t, a)
	b := browser.Start(t)
	// Painters in their colours; the last one paints stars while the
	// shots are taken.
	var painter *browser.Page
	for _, color := range []string{"14", "13", "6"} {
		painter = b.NewPage(t, 800, 600)
		painter.Navigate(t, base+"/demos/canvas")
		painter.Wait(t, `document.querySelector('.live-badge.is-live') && document.querySelector('#canvas-board[palette]')`, 10*time.Second)
		painter.Eval(t, `document.querySelector('.cv__swatch input[value="`+color+`"]').click()`, nil)
	}
	stars := canvasPenOn(t, painter)
	twinkle := func() {
		for _, s := range [][2]int{{14, 30}, {50, 18}, {23, 8}, {60, 40}, {36, 5}} {
			stars.down(s[0], s[1])
			stars.up(s[0], s[1])
		}
	}
	shot := func(name string, w, h int, cookie string, full bool, setup ...string) {
		p := b.NewPage(t, w, h)
		defer p.Close(t)
		p.Call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "reduce"}}})
		if cookie != "" {
			p.Navigate(t, base+"/")
			p.Eval(t, "document.cookie = '"+cookie+"; Path=/'", nil)
		}
		p.Navigate(t, base+"/demos/canvas")
		p.Eval(t, "document.fonts.ready.then(() => true)", nil)
		p.Wait(t, `document.querySelector('#canvas-board[palette]') && document.querySelector('.live-badge.is-live')`, 10*time.Second)
		for _, js := range setup {
			p.Eval(t, js, nil)
		}
		twinkle()
		time.Sleep(900 * time.Millisecond)
		p.Screenshot(t, filepath.Join(dir, name+".png"), full)
		p.ShotElement(t, ".demo-stage", filepath.Join(dir, name+"-stage.png"), 1.5)
		if errs := p.Errors(); len(errs) > 0 {
			t.Errorf("%s: %v", name, errs)
		}
	}
	head := b.NewPage(t, 1440, 900)
	head.Navigate(t, base+"/demos/canvas")
	head.ShotElement(t, ".demo-head__art", filepath.Join(dir, "canvas-art.png"), 3)
	head.Close(t)
	shot("canvas", 1440, 900, "", false)
	shot("canvas-light", 1440, 900, "lith-theme=light", false)
	shot("canvas-smooth", 1440, 900, "lith-style=smooth", false)
	shot("mobile-canvas", 390, 844, "", true)
	shot("canvas-how", 1440, 900, "", true, `document.querySelector('.howto').open = true`)

	shot("tablet-canvas", 820, 1180, "", false)

	// The theme switch: the circle opens on the canvas already in the
	// light theme's palette.
	vt := b.NewPage(t, 1280, 800)
	vt.Navigate(t, base+"/demos/canvas")
	vt.Wait(t, `document.querySelector('#canvas-board[palette]') && document.querySelector('.live-badge.is-live')`, 10*time.Second)
	vt.Eval(t, "document.fonts.ready.then(() => true)", nil)
	time.Sleep(800 * time.Millisecond)
	vt.Call(t, "Animation.enable", nil)
	vt.Call(t, "Animation.setPlaybackRate", map[string]any{"playbackRate": 0.1})
	vt.Eval(t, `document.querySelector('.prefs-button').click(); document.querySelector('[data-pref="theme"][data-value="light"]').click()`, nil)
	for i := range 4 {
		time.Sleep(900 * time.Millisecond)
		vt.Call(t, "Animation.setPlaybackRate", map[string]any{"playbackRate": 0.1})
		vt.Screenshot(t, filepath.Join(dir, fmt.Sprintf("vt-canvas-theme-%d.png", i)), false)
	}
	vt.Call(t, "Animation.setPlaybackRate", map[string]any{"playbackRate": 1})

	// A stroke of your own under way: the frame in your brush's colour.
	p := b.NewPage(t, 1440, 900)
	p.Navigate(t, base+"/demos/canvas")
	p.Wait(t, `document.querySelector('#canvas-board[palette]') && document.querySelector('.live-badge.is-live')`, 10*time.Second)
	p.Eval(t, `document.querySelector('.cv__swatch input[value="11"]').click()`, nil)
	pn := canvasPenOn(t, p)
	pn.down(40, 30)
	for i := 1; i <= 8; i++ {
		pn.to(40+i, 30-i/2)
	}
	twinkle()
	time.Sleep(700 * time.Millisecond)
	p.ShotElement(t, ".demo-stage", filepath.Join(dir, "canvas-stroke.png"), 1.5)
	pn.up(48, 26)
}

// TestCanvas paints over HTTP and reads the outcome from the stream: the
// board with the new cells, the painter in the brush's colour, the pulse
// with the pixels. The pixel limit refuses a second burst of 60, and a
// paint that is not one gets 400.
func TestCanvas(t *testing.T) {
	_, base := apptest.Start(t)
	c := client(t)
	get(t, c, base+"/demos/canvas")
	events := listen(t, c, base+"/demos/canvas/stream?tab=tab1")
	next(t, events, `id="canvas-board"`)
	next(t, events, `id="canvas-pulse"`)

	if code := post(t, c, base+"/cmd/canvas/pick", `{"tab":"tab1","color":14}`); code != http.StatusNoContent {
		t.Fatalf("pick: %d", code)
	}
	next(t, events, `data-c="14"`)
	if code := post(t, c, base+"/cmd/canvas/paint", `{"tab":"tab1","color":7,"cells":[2,1,0,0]}`); code != http.StatusNoContent {
		t.Fatalf("paint: %d", code)
	}
	// One tick's frames come in the order the stream watches the views:
	// the board, the painters (the paint's colour is the painter's now),
	// the pulse.
	board := next(t, events, `cells="7770`)
	if strings.Contains(board, "tab1") {
		t.Error("the board's frame carries the tab id")
	}
	next(t, events, `data-c="7"`)
	next(t, events, `--canvas-n: 3`)

	for body, want := range map[string]int{
		`{"tab":"tab1","color":16,"cells":[1]}`:   http.StatusBadRequest,
		`{"tab":"tab1","color":1,"cells":[4096]}`: http.StatusBadRequest,
		`{"tab":"tab1","color":1,"cells":[]}`:     http.StatusBadRequest,
		`{"tab":"","color":1,"cells":[1]}`:        http.StatusBadRequest,
		`{"tab":"tab1","color":1,"cells":"x"}`:    http.StatusBadRequest,
	} {
		if code := post(t, c, base+"/cmd/canvas/paint", body); code != want {
			t.Errorf("%s: %d, want %d", body, code, want)
		}
	}
	cells := make([]string, 60)
	for i := range cells {
		cells[i] = fmt.Sprint(100 + i)
	}
	// A new session has a full bucket: 60 pixels at once, then 20 a second.
	c2 := client(t)
	get(t, c2, base+"/demos/canvas")
	sixty := `{"tab":"tab2","color":3,"cells":[` + strings.Join(cells, ",") + `]}`
	first, second := post(t, c2, base+"/cmd/canvas/paint", sixty), post(t, c2, base+"/cmd/canvas/paint", sixty)
	if first != http.StatusNoContent || second != http.StatusTooManyRequests {
		t.Errorf("two bursts of 60 pixels: %d, %d", first, second)
	}
}

// TestCanvasWire measures what painting costs on the wire: a painter's
// stream with zstd and with brotli, a pixel painted at a time, a tick
// apart. Each paint changes the board and the pulse; their frames go out
// together in one compressed write.
//
//	go test ./internal/app -run TestCanvasWire -v
func TestCanvasWire(t *testing.T) {
	a, base := apptest.Start(t)
	paintCanvasScene(t, a)
	c := client(t)
	get(t, c, base+"/demos/canvas")
	const paints = 40
	for _, enc := range []string{"identity", "zstd", "br"} {
		w := openCanvasRaw(t, c, base+"/demos/canvas/stream?tab=wire"+enc, enc)
		time.Sleep(300 * time.Millisecond) // the first frames
		start := w.bytes()
		for i := range paints {
			body := fmt.Sprintf(`{"tab":"wire%s","color":%d,"cells":[%d]}`, enc, 1+i%15, 2000+i)
			if code := post(t, c, base+"/cmd/canvas/paint", body); code != http.StatusNoContent {
				t.Fatalf("paint: %d", code)
			}
			time.Sleep(120 * time.Millisecond)
		}
		per := (w.bytes() - start) / paints
		t.Logf("%s: %d bytes per paint, %d bytes for the first frames", enc, per, start)
		if enc != "identity" && per > 300 {
			t.Errorf("%s: %d bytes per paint", enc, per)
		}
		w.close()
	}
}

type canvasRawStream struct {
	mu     sync.Mutex
	n      int
	cancel context.CancelFunc
}

func (s *canvasRawStream) bytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func (s *canvasRawStream) close() { s.cancel() }

// openCanvasRaw opens a stream with the encoding and counts the bytes that
// arrive, as they are on the wire.
func openCanvasRaw(t *testing.T, c *http.Client, url, encoding string) *canvasRawStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Accept-Encoding", encoding)
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || (encoding != "identity" && res.Header.Get("Content-Encoding") != encoding) {
		t.Fatalf("stream: %d, %q", res.StatusCode, res.Header.Get("Content-Encoding"))
	}
	s := &canvasRawStream{cancel: cancel}
	go func() {
		defer res.Body.Close()
		buf := make([]byte, 32<<10)
		for {
			n, err := res.Body.Read(buf)
			s.mu.Lock()
			s.n += n
			s.mu.Unlock()
			if err != nil {
				if err != io.EOF && ctx.Err() == nil {
					t.Log(err)
				}
				return
			}
		}
	}()
	return s
}

// TestCanvasBrowser drives the canvas in Chromium with two people, each
// in a browser context of their own. The palette comes from the site's
// tokens; Ada picks coral and Bob sees her face turn coral; Ada paints a
// stroke with the mouse and Bob's board, pulse and list show it; Bob
// paints from the keyboard and Ada sees it; Bob's light theme reads the
// palette again; Ada leaves and Bob's list loses her. The consoles stay
// empty.
//
//	LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestCanvasBrowser
func TestCanvasBrowser(t *testing.T) {
	a, base := apptest.Start(t)
	b := browser.Start(t)
	ada := b.NewPage(t, 1280, 900)
	ada.Navigate(t, base+"/demos/canvas")
	bob := b.NewPage(t, 1280, 900)
	bob.Navigate(t, base+"/demos/canvas")
	for _, p := range []*browser.Page{ada, bob} {
		p.Wait(t, `document.querySelector('.live-badge.is-live') && document.querySelector('#canvas-board[palette]')`, 10*time.Second)
	}
	waitFor(t, "two painters", func() bool { return a.Hub().Count("canvas:here") == 2 })
	bob.Wait(t, `document.querySelectorAll('#canvas-here .cv__painter').length === 2`, 5*time.Second)

	var palette []string
	ada.Eval(t, `JSON.parse(document.querySelector('#canvas-board').getAttribute('palette'))`, &palette)
	if len(palette) != 16 || palette[0] != "#0a0806" || palette[1] != "#f2f0ec" || palette[7] != "#f48e1f" {
		t.Errorf("the palette from the dark theme's tokens: %v", palette)
	}

	var me string
	ada.Eval(t, `document.querySelector('.cv style').textContent.match(/data-tag="([0-9a-f]+)"/)[1]`, &me)
	ada.Eval(t, `document.querySelector('.cv__swatch input[value="14"]').click()`, nil)
	bob.Wait(t, `document.querySelector('#canvas-painter-`+me+`')?.dataset.c === '14'`, 5*time.Second)

	// A stroke across eleven cells of row 10, with the mouse.
	pn := canvasPenOn(t, ada)
	pn.down(10, 10)
	for x := 11; x <= 20; x++ {
		pn.to(x, 10)
	}
	pn.up(20, 10)
	const stroke = `document.querySelector('#canvas-board').getAttribute('cells').slice(650, 661) === 'eeeeeeeeeee'`
	bob.Wait(t, stroke, 5*time.Second)
	ada.Wait(t, stroke, 5*time.Second)
	bob.Wait(t, `document.querySelector('#canvas-pulse .cv__n').textContent === '11'`, 5*time.Second)
	bob.Wait(t, `document.querySelector('#canvas-painter-`+me+` .cv__painted')?.textContent === '11'`, 5*time.Second)
	// The board's frames left the page's palette and brush alone.
	var kept bool
	ada.Eval(t, `(b => b.hasAttribute('palette') && b.getAttribute('color') === '14' && getComputedStyle(b).opacity === '1')(document.querySelector('#canvas-board'))`, &kept)
	if !kept {
		t.Error("a frame of the board took the page's palette or brush")
	}

	// A long stroke at once is more than the burst of 60 pixels: the
	// server refuses the rest, and the page says why.
	pn.down(0, 40)
	for x := 1; x < 64; x++ {
		pn.to(x, 40)
	}
	for x := 63; x >= 0; x -= 2 {
		pn.to(x, 41)
	}
	pn.up(0, 41)
	ada.Wait(t, `document.querySelector('.cv__problem').textContent.includes('20 a second')`, 5*time.Second)
	pn.down(5, 5) // a new stroke clears it
	pn.up(5, 5)
	ada.Wait(t, `document.querySelector('.cv__problem').textContent === ''`, 2*time.Second)

	// Bob paints from the keyboard: Space paints the cursor's cell, which
	// starts in the middle, in his brush.
	bob.Eval(t, `document.querySelector('#canvas-board').shadowRoot.querySelector('canvas').focus()`, nil)
	for _, typ := range []string{"keyDown", "keyUp"} {
		bob.Call(t, "Input.dispatchKeyEvent", map[string]any{"type": typ, "key": " ", "code": "Space", "windowsVirtualKeyCode": 32})
	}
	ada.Wait(t, `document.querySelector('#canvas-board').getAttribute('cells')[32 * 64 + 32] === '7'`, 5*time.Second)

	// The light theme: the page reads the palette again, and the
	// background is the light theme's inset surface.
	bob.Eval(t, `document.querySelector('.prefs-button').click()`, nil)
	bob.Wait(t, `document.querySelector('#prefs').matches(':popover-open')`, 2*time.Second)
	bob.Eval(t, `document.querySelector('[data-pref="theme"][data-value="light"]').click()`, nil)
	bob.Wait(t, `JSON.parse(document.querySelector('#canvas-board').getAttribute('palette'))[0] === '#f1eeea'`, 3*time.Second)

	ada.Close(t)
	bob.Wait(t, `document.querySelectorAll('#canvas-here .cv__painter').length === 1`, 5*time.Second)

	for name, p := range map[string]*browser.Page{"Ada's canvas": ada, "Bob's canvas": bob} {
		for _, e := range p.Errors() {
			// The refused paints of the long stroke, which Chromium logs.
			if strings.Contains(e, "status of 429") && strings.Contains(e, "/cmd/canvas/paint") {
				continue
			}
			t.Errorf("%s: %s", name, e)
		}
	}
}
