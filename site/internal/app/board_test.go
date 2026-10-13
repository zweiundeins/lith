package app_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
)

// TestBoardStream reads the shared board's stream over HTTP: the board's
// first frame without a view transition, a move's frame inside one with
// the board's version, the mover's outcome on its own tab only, a stale
// move refused, and the presence row once a tab says hello.
func TestBoardStream(t *testing.T) {
	_, base := apptest.Start(t)
	c := client(t)
	get(t, c, base+"/demos/board")

	events := listen(t, c, base+"/demos/board/stream?tab=tab1")
	first := next(t, events, `id="board-lanes"`)
	if strings.Contains(first, "useViewTransition") {
		t.Errorf("the board's first frame animates:\n%s", first)
	}
	next(t, events, `"_board":{"v":0}`)
	next(t, events, `"ack":""`)

	if code := post(t, c, base+"/cmd/board/hello", `{"tab":"tab1"}`); code != http.StatusNoContent {
		t.Fatalf("hello: %d", code)
	}
	next(t, events, `class="bd-av"`)

	// A second tab of the same session, to hear its own outcome.
	other := listen(t, c, base+"/demos/board/stream?tab=tab2")
	next(t, other, `"ack":""`)

	if code := post(t, c, base+"/cmd/board/move", `{"tab":"tab1","op":"op1","card":4,"lane":1,"index":0,"seen":0}`); code != http.StatusNoContent {
		t.Fatalf("move: %d", code)
	}
	board := next(t, events, `data-card="4" data-version="1"`)
	if !strings.Contains(board, "useViewTransition true") || !strings.Contains(board, "view-transition-class: board-card board-moved") {
		t.Errorf("a move's frame does not animate:\n%s", board)
	}
	next(t, events, `"_board":{"v":1}`)
	next(t, events, `"ack":"op1","ackv":1,"ok":true`)
	next(t, other, `"_board":{"v":1}`)

	// The second tab moves the same card from the version it had: refused,
	// and only that tab hears it.
	if code := post(t, c, base+"/cmd/board/move", `{"tab":"tab2","op":"op2","card":4,"lane":2,"index":0,"seen":0}`); code != http.StatusNoContent {
		t.Fatalf("a stale move: %d", code)
	}
	refusal := next(t, other, `id="board-outcome"`)
	if !strings.Contains(refusal, "You moved “Tune the tick” in another tab first. It is in Doing now.") {
		t.Errorf("the refusal:\n%s", refusal)
	}
	next(t, other, `"ack":"op2","ackv":1,"ok":false`)

	// The landing page's stream carries the board in miniature.
	home := listen(t, c, base+"/stream?tab=tab3")
	next(t, home, `id="board-mini-4-1"`)

	// The numbers: one render of the board for the move, a frame to each
	// of the two tabs.
	stats := next(t, events, `<dd>1</dd>`)
	if !strings.Contains(stats, `<dd>2</dd>`) {
		t.Errorf("the numbers after the move:\n%s", stats)
	}
}

// TestBoardActions checks the edge in front of the board's actions: a bad
// payload, a bad tab, a burst beyond the rate.
func TestBoardActions(t *testing.T) {
	_, base := apptest.Start(t)
	c := client(t)
	get(t, c, base+"/demos/board")
	for body, want := range map[string]int{
		`{"tab":"bad tab!","op":"o","card":1,"lane":1,"index":0,"seen":0}`: http.StatusBadRequest,
		`{"tab":"t1","op":"bad op!","card":1,"lane":1,"index":0,"seen":0}`: http.StatusBadRequest,
		`{"tab":"t1","op":"o1","card":1,"lane":7,"index":0,"seen":0}`:      http.StatusBadRequest,
		`not json`: http.StatusBadRequest,
	} {
		if code := post(t, c, base+"/cmd/board/move", body); code != want {
			t.Errorf("%s: %d, want %d", body, code, want)
		}
	}
	codes := map[int]int{}
	for i := range 12 {
		codes[post(t, c, base+"/cmd/board/move", fmt.Sprintf(`{"tab":"t1","op":"burst%d","card":1,"lane":0,"index":0,"seen":0}`, i))]++
	}
	// Four bad requests took tokens of the burst of eight.
	if codes[http.StatusNoContent] == 0 || codes[http.StatusTooManyRequests] == 0 {
		t.Errorf("a burst of moves: %v", codes)
	}
}

// TestBoard drives the board in Chromium, in two browser contexts, so two
// sessions: Ada moves a card with the keyboard and one with the mouse, Bob
// sees both glide into place; a phone drags a card by its grip; Bob drops
// a card Ada moved while he held it, and only Bob hears why. The landing
// page's miniature follows, and no console reports an error.
//
//	LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestBoard
func TestBoard(t *testing.T) {
	a, base := apptest.Start(t)
	b := browser.Start(t)
	ada := b.NewPage(t, 1280, 1000)
	bob := b.NewPage(t, 1280, 1000)
	for _, p := range []*browser.Page{ada, bob} {
		p.Navigate(t, base+"/demos/board")
		p.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)
	}
	waitFor(t, "two tabs on the board", func() bool { return a.Hub().Count("board:board") == 2 })
	for _, p := range []*browser.Page{ada, bob} {
		// Both people on the row, and each page marks its own.
		p.Wait(t, `document.querySelectorAll('#board-here .bd-av').length === 2`, 5*time.Second)
		var mine int
		p.Eval(t, `[...document.querySelectorAll('#board-here .bd-av')].filter((a) => getComputedStyle(a).getPropertyValue('--bd-me').trim() === '1').length`, &mine)
		if mine != 1 {
			t.Errorf("%d avatars marked as the viewer's own", mine)
		}
	}

	// The keyboard: Tune the tick, from to do into doing, second place.
	boardFocus(t, ada, 4)
	boardPress(t, ada, " ")
	ada.Wait(t, `document.querySelector('[aria-live="assertive"]').textContent.startsWith('Picked up “Tune the tick”: To do, 4 of 5.')`, 2*time.Second)
	boardPress(t, ada, "ArrowRight")
	ada.Wait(t, `document.querySelector('[aria-live="assertive"]').textContent.startsWith('Doing, 4 of 4.')`, 2*time.Second)
	boardPress(t, ada, "ArrowUp")
	boardPress(t, ada, "ArrowUp")
	ada.Wait(t, `document.querySelector('[aria-live="assertive"]').textContent.startsWith('Doing, 2 of 4.')`, 2*time.Second)
	var anchored bool
	ada.Eval(t, `getComputedStyle(document.querySelector('[data-lane="1"] .bd-slot:nth-child(2)')).anchorName === '--bd-slot'`, &anchored)
	if !anchored {
		t.Error("the marker is not anchored to the place the card would go")
	}
	boardPress(t, ada, " ")
	ada.Wait(t, `document.querySelector('#board-outcome').textContent.includes('You moved “Tune the tick” to Doing.')`, 5*time.Second)
	bob.Wait(t, `document.querySelector('[data-lane="1"] .bd-slot:nth-child(2)')?.id === 'board-card-4'`, 5*time.Second)
	ada.Wait(t, `document.activeElement === document.querySelector('#board-card-4 button')`, 2*time.Second)
	var outcome string
	bob.Eval(t, `document.querySelector('#board-outcome').textContent.trim()`, &outcome)
	if outcome != "" {
		t.Errorf("Bob hears %q about Ada's move", outcome)
	}

	// A card picked up goes back when the focus leaves it.
	boardFocus(t, ada, 6)
	boardPress(t, ada, " ")
	ada.Eval(t, `document.querySelector('.howto summary').focus()`, nil)
	ada.Wait(t, `document.querySelector('[aria-live="assertive"]').textContent.startsWith('“Render a shared view” stays in Doing.') && !document.querySelector('style[data-text]').textContent.includes('--bd-pick')`, 2*time.Second)

	// The mouse: Open a stream, from done to the top of to do.
	// Straight after the move, while its view transition still runs: the
	// cards take the pointer through the transition's overlay.
	var active bool
	ada.Eval(t, `document.documentElement.matches(':active-view-transition')`, &active)
	t.Logf("a view transition runs as the boardDrag starts: %v", active)
	boardDrag(t, ada, "#board-card-9", "#board-card-1", 0.2)
	bob.Wait(t, `document.querySelector('[data-lane="0"] .bd-slot')?.id === 'board-card-9'`, 5*time.Second)
	ada.Wait(t, `!document.querySelector('.bd').classList.contains('is-dragging') && document.querySelector('.bd-ghost').style.display === 'none'`, 2*time.Second)

	// A phone drags Set up the sweep by its grip, from done into doing.
	phone := b.NewPage(t, 390, 844)
	phone.Call(t, "Emulation.setTouchEmulationEnabled", map[string]any{"enabled": true, "maxTouchPoints": 5})
	phone.Call(t, "Emulation.setDeviceMetricsOverride", map[string]any{"width": 390, "height": 844, "deviceScaleFactor": 2, "mobile": true})
	phone.Navigate(t, base+"/demos/board")
	phone.Wait(t, `document.querySelector('.live-badge.is-live') && matchMedia('(pointer: coarse)').matches`, 10*time.Second)
	boardTouchDrag(t, phone, "#board-card-10 .bd-card__grip", `[data-lane="1"] .bd-slot:last-child`)
	bob.Wait(t, `document.querySelector('#board-card-10')?.closest('[data-lane]')?.dataset.lane === '1'`, 5*time.Second)

	// Bob picks up Sketch the monolith; Ada moves it to done meanwhile; Bob
	// drops it in doing: refused, and only Bob hears why.
	boardFocus(t, bob, 1)
	boardPress(t, bob, " ")
	boardFocus(t, ada, 1)
	boardPress(t, ada, " ")
	boardPress(t, ada, "ArrowRight")
	boardPress(t, ada, "ArrowRight")
	boardPress(t, ada, " ")
	bob.Wait(t, `document.querySelector('#board-card-1')?.closest('[data-lane]')?.dataset.lane === '2'`, 5*time.Second)
	boardPress(t, bob, "ArrowRight")
	boardPress(t, bob, " ")
	bob.Wait(t, `document.querySelector('#board-outcome.is-refused')?.textContent.includes('moved “Sketch the monolith” first. It is in Done now.')`, 5*time.Second)
	bob.Wait(t, `!document.querySelector('style[data-text]').textContent.includes('--bd-wait')`, 2*time.Second)
	ada.Eval(t, `document.querySelector('#board-outcome').textContent`, &outcome)
	if !strings.Contains(outcome, "You moved “Sketch the monolith” to Done.") {
		t.Errorf("Ada's outcome: %q", outcome)
	}

	// The landing page's miniature, and the numbers.
	home := b.NewPage(t, 1280, 900)
	home.Navigate(t, base+"/")
	home.Wait(t, `document.querySelector('#board-mini .bd-mini__lane [id^="board-mini-9-"]')`, 5*time.Second)
	home.Wait(t, `document.querySelector('[data-demo="board"] .bd-embed__head.is-live')`, 10*time.Second)
	ada.Wait(t, `document.querySelector('#board-stats .bd-tick__step--render dd')?.textContent === '1'`, 5*time.Second)

	for name, p := range map[string]*browser.Page{"Ada": ada, "Bob": bob, "the phone": phone, "the landing page": home} {
		if errs := p.Errors(); len(errs) > 0 {
			t.Errorf("%s: %d errors:\n%v", name, len(errs), errs)
		}
	}
}

// boardFocus focuses a card's button.
func boardFocus(t *testing.T, p *browser.Page, card int) {
	t.Helper()
	p.Eval(t, fmt.Sprintf(`document.querySelector('#board-card-%d button').focus()`, card), nil)
}

// boardPress sends a key to the page's focused element.
func boardPress(t *testing.T, p *browser.Page, key string) {
	t.Helper()
	codes := map[string]struct {
		code string
		vk   int
	}{" ": {"Space", 32}, "Enter": {"Enter", 13}, "Escape": {"Escape", 27}, "ArrowLeft": {"ArrowLeft", 37}, "ArrowUp": {"ArrowUp", 38}, "ArrowRight": {"ArrowRight", 39}, "ArrowDown": {"ArrowDown", 40}}
	k := codes[key]
	for _, typ := range []string{"rawKeyDown", "keyUp"} {
		p.Call(t, "Input.dispatchKeyEvent", map[string]any{"type": typ, "key": key, "code": k.code, "windowsVirtualKeyCode": k.vk})
	}
	time.Sleep(30 * time.Millisecond)
}

type boardBox struct{ X, Y, W, H float64 }

func boardRect(t *testing.T, p *browser.Page, sel string) boardBox {
	t.Helper()
	var r boardBox
	p.Eval(t, `(() => { const b = document.querySelector(`+jsQuote(sel)+`).getBoundingClientRect(); return {x: b.left, y: b.top, w: b.width, h: b.height} })()`, &r)
	return r
}

// boardDrag drags from the middle of one element to another, at fy of its
// height, in small steps, with the mouse.
func boardDrag(t *testing.T, p *browser.Page, from, to string, fy float64) {
	t.Helper()
	p.Eval(t, `document.querySelector('.bd__board').scrollIntoView({block: 'center', behavior: 'instant'})`, nil)
	s := boardRect(t, p, from)
	d := boardRect(t, p, to)
	sx, sy, tx, ty := s.X+s.W/2, s.Y+s.H/2, d.X+d.W/2, d.Y+d.H*fy
	mouse := func(typ string, x, y float64) {
		p.Call(t, "Input.dispatchMouseEvent", map[string]any{"type": typ, "x": x, "y": y, "button": "left", "buttons": 1, "clickCount": 1})
	}
	mouse("mousePressed", sx, sy)
	for i := 1; i <= 10; i++ {
		f := float64(i) / 10
		mouse("mouseMoved", sx+(tx-sx)*f, sy+(ty-sy)*f)
		time.Sleep(15 * time.Millisecond)
	}
	mouse("mouseReleased", tx, ty)
}

// boardTouchDrag drags with a finger from one element to below another.
func boardTouchDrag(t *testing.T, p *browser.Page, from, to string) {
	t.Helper()
	p.Eval(t, `document.querySelector('.bd__board').scrollIntoView({block: 'center', behavior: 'instant'})`, nil)
	s := boardRect(t, p, from)
	d := boardRect(t, p, to)
	sx, sy, tx, ty := s.X+s.W/2, s.Y+s.H/2, d.X+d.W*0.8, d.Y+d.H*0.6
	touch := func(typ string, x, y float64) {
		points := []map[string]any{{"x": x, "y": y}}
		if typ == "touchEnd" {
			points = []map[string]any{}
		}
		p.Call(t, "Input.dispatchTouchEvent", map[string]any{"type": typ, "touchPoints": points})
	}
	touch("touchStart", sx, sy)
	for i := 1; i <= 10; i++ {
		f := float64(i) / 10
		touch("touchMove", sx+(tx-sx)*f, sy+(ty-sy)*f)
		time.Sleep(15 * time.Millisecond)
	}
	touch("touchEnd", tx, ty)
}
