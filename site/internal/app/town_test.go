package app_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
)

// TestTownStream reads the town's stream over HTTP: the tab's view with
// the hello, the slider's ends, the town now once the page says hello, a
// seek that moves the stream to the town as of a position, a change from
// there refused, a change from now that reaches the slider's end but not
// the past, and Now again.
func TestTownStream(t *testing.T) {
	a, base := apptest.Start(t)
	c := client(t)
	get(t, c, base+"/demos/town")

	events := listen(t, c, base+"/demos/town/stream?tab=tab1")
	townNext(t, events, `"hello":true`, `"head":36`) // the starting town
	if code := post(t, c, base+"/cmd/town/follow", `{"tab":"tab1"}`); code != http.StatusNoContent {
		t.Fatalf("follow: %d", code)
	}
	now := next(t, events, `id="town-map" class="town-map" data-at="0"`)
	if !strings.Contains(now, `data-t="44" data-k="bridge" data-v="2"`) || !strings.Contains(now, `id="town-later"`) {
		t.Errorf("the town now:\n%s", now)
	}

	// Back to position 9: the river without its bridge, no houses.
	if code := post(t, c, base+"/cmd/town/seek", `{"tab":"tab1","at":9,"seq":1}`); code != http.StatusNoContent {
		t.Fatalf("seek: %d", code)
	}
	past := townNext(t, events, `data-at="9"`, `{"_town":{"at":9,"seq":1,"ack":"","ok":false}}`, `id="town-later"`)
	if !strings.Contains(past, `data-t="44" data-k="water" aria-disabled="true"`) || strings.Contains(past, `data-k="house"`) {
		t.Errorf("the town at 9:\n%s", past)
	}
	if c := a.Hub().Count("town:now"); c != 0 {
		t.Errorf("the tab in the past still watches the town now: %d", c)
	}

	// A change from the past: refused.
	if code := post(t, c, base+"/cmd/town/place", `{"tab":"tab1","op":"op1","tile":0,"kind":"house","seen":0}`); code != http.StatusNoContent {
		t.Fatalf("place: %d", code)
	}
	next(t, events, "This tab shows the past.")

	// Another tab builds: the slider's end moves, the past stays.
	other := listen(t, c, base+"/demos/town/stream?tab=tab2")
	next(t, other, `"hello":true`)
	post(t, c, base+"/cmd/town/follow", `{"tab":"tab2"}`)
	next(t, other, `data-at="0"`)
	if code := post(t, c, base+"/cmd/town/place", `{"tab":"tab2","op":"op2","tile":0,"kind":"house","seen":0}`); code != http.StatusNoContent {
		t.Fatalf("place: %d", code)
	}
	next(t, other, `data-t="0" data-k="house" data-v="1"`)
	next(t, events, `"head":37`)
	if n := a.Hub().Stats().RendersByKey["town:at:9"]; n != 1 {
		t.Errorf("the town at 9 rendered %d times", n)
	}

	// Now: live again, with the new house.
	post(t, c, base+"/cmd/town/seek", `{"tab":"tab1","at":0,"seq":2}`)
	next(t, events, `data-t="0" data-k="house" data-v="1"`)
}

// TestTownActions checks the edge in front of the town's actions.
func TestTownActions(t *testing.T) {
	_, base := apptest.Start(t)
	c := client(t)
	get(t, c, base+"/demos/town")
	listen(t, c, base+"/demos/town/stream?tab=t1")
	for body, want := range map[string]int{
		`{"tab":"bad tab!","op":"o","tile":1,"kind":"house","seen":0}`: http.StatusBadRequest,
		`{"tab":"t1","op":"o1","tile":96,"kind":"house","seen":0}`:     http.StatusBadRequest,
		`{"tab":"t1","op":"o2","tile":1,"kind":"castle","seen":0}`:     http.StatusBadRequest,
		`not json`: http.StatusBadRequest,
	} {
		if code := post(t, c, base+"/cmd/town/place", body); code != want {
			t.Errorf("%s: %d, want %d", body, code, want)
		}
	}
	if code := post(t, c, base+"/cmd/town/seek", `{"tab":"t1","at":3,"seq":0}`); code != http.StatusBadRequest {
		t.Errorf("a seek without its number: %d", code)
	}
	codes := map[int]int{}
	for i := range 16 {
		codes[post(t, c, base+"/cmd/town/place", fmt.Sprintf(`{"tab":"t1","op":"burst%d","tile":%d,"kind":"tree","seen":0}`, i, 60+i))]++
	}
	if codes[http.StatusNoContent] == 0 || codes[http.StatusTooManyRequests] == 0 {
		t.Errorf("a burst of changes: %v", codes)
	}
}

// TestTown drives the town in Chromium, in two browser contexts, so two
// sessions: Ada builds a house that Bob sees arrive; Bob drags the slider
// back and sees the town as it was, his tiles closed to changes and his
// badge saying past; Ada plants a tree, which moves the end of Bob's
// slider and nothing else he sees; Bob presses Now and is live again, and
// sees Ada's next change. No console reports an error.
//
//	LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run 'TestTown$'
func TestTown(t *testing.T) {
	a, base := apptest.Start(t)
	b := browser.Start(t)
	ada := b.NewPage(t, 1280, 1000)
	bob := b.NewPage(t, 1280, 1000)
	for _, p := range []*browser.Page{ada, bob} {
		p.Navigate(t, base+"/demos/town")
		p.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)
	}
	waitFor(t, "two tabs on the town now", func() bool { return a.Hub().Count("town:now") == 2 })
	head := 36 // the starting town's events

	// Ada builds a house on A1, grass in the starting town.
	townClick(t, ada, 0)
	for _, p := range []*browser.Page{ada, bob} {
		p.Wait(t, `document.querySelector('#town-map [data-t="0"]').dataset.k === 'house'`, 5*time.Second)
		p.Wait(t, fmt.Sprintf(`document.querySelector('.town-slider').max === '%d'`, head+1), 5*time.Second)
	}
	ada.Wait(t, `!document.querySelector('style[data-text]').textContent`, 2*time.Second)

	// Bob drags the slider to its start, then steps to position 9 with the
	// keyboard: the river without its bridge, no houses yet.
	townDrag(t, bob, 0)
	bob.Wait(t, `document.querySelector('#town-map').dataset.at === '1'`, 5*time.Second)
	for range 8 {
		townPress(t, bob, "ArrowRight")
	}
	bob.Wait(t, `document.querySelector('#town-map').dataset.at === '9'`, 5*time.Second)
	bob.Wait(t, `document.querySelector('.live-badge').textContent === 'past'`, 2*time.Second)
	var past struct {
		Bridge, House, Water, Disabled, Hint, Here, Badge string
		Tools                                             bool
	}
	bob.Eval(t, `(() => {
		const k = (t) => document.querySelector('#town-map [data-t="' + t + '"]').dataset.k
		return {Bridge: k(44), House: k(0), Water: k(91), Disabled: document.querySelector('#town-map [data-t="5"]').getAttribute('aria-disabled'),
			Hint: getComputedStyle(document.querySelector('.town-past')).display, Here: document.querySelector('#town-earlier [data-here]').textContent,
			Badge: document.querySelector('.live-badge').className, Tools: document.querySelector('.town-tool input').disabled}
	})()`, &past)
	if past.Bridge != "water" || past.House != "grass" || past.Water != "water" || past.Disabled != "true" || past.Hint == "none" || !past.Tools ||
		!strings.Contains(past.Here, "The town flooded H8") || past.Badge != "live-badge is-held" {
		t.Errorf("Bob's town at 9: %+v", past)
	}
	var adaBadge string
	ada.Eval(t, `document.querySelector('.live-badge').textContent + ' ' + document.querySelector('.live-badge').className`, &adaBadge)
	if adaBadge != "live live-badge is-live" {
		t.Errorf("Ada's badge, at now: %s", adaBadge)
	}
	// A click on the past changes nothing.
	townClick(t, bob, 5)
	time.Sleep(200 * time.Millisecond)

	// Ada plants a tree on B1: Bob's slider reaches further, his town
	// stays at 9 and renders no more.
	townTool(t, ada, "tree")
	townClick(t, ada, 1)
	ada.Wait(t, `document.querySelector('#town-map [data-t="1"]').dataset.k === 'tree'`, 5*time.Second)
	bob.Wait(t, fmt.Sprintf(`document.querySelector('.town-slider').max === '%d'`, head+2), 5*time.Second)
	var still string
	bob.Eval(t, `document.querySelector('#town-map').dataset.at + ' ' + document.querySelector('#town-map [data-t="1"]').dataset.k + ' ' + document.querySelector('#town-map [data-t="5"]').dataset.k`, &still)
	if still != "9 grass grass" {
		t.Errorf("Bob's town in the past after Ada's tree: %s", still)
	}
	if n := a.Hub().Stats().RendersByKey["town:at:9"]; n != 1 {
		t.Errorf("the town at 9 rendered %d times", n)
	}
	if dir := os.Getenv("LITH_SITE_SHOTS"); dir != "" {
		bob.Eval(t, `document.querySelector('.demo-stage').scrollIntoView({block: 'start', behavior: 'instant'})`, nil)
		time.Sleep(300 * time.Millisecond)
		bob.Screenshot(t, filepath.Join(dir, "town-past.png"), false)
		ada.Eval(t, `document.querySelector('.demo-stage').scrollIntoView({block: 'start', behavior: 'instant'})`, nil)
		time.Sleep(300 * time.Millisecond)
		ada.Screenshot(t, filepath.Join(dir, "town-live.png"), false)
	}

	// Now: Bob is live again, and sees Ada's next change.
	bob.Eval(t, `document.querySelector('.town-time .btn').click()`, nil)
	bob.Wait(t, `document.querySelector('#town-map').dataset.at === '0' && document.querySelector('#town-map [data-t="1"]').dataset.k === 'tree'`, 5*time.Second)
	bob.Wait(t, `getComputedStyle(document.querySelector('.town-past')).display === 'none' && document.querySelector('.town-slider').value === document.querySelector('.town-slider').max`, 2*time.Second)
	bob.Wait(t, `document.querySelector('.live-badge.is-live:not(.is-held)').textContent === 'live'`, 2*time.Second)
	townTool(t, ada, "water")
	townClick(t, ada, 2) // a tree in the starting town
	bob.Wait(t, `document.querySelector('#town-map [data-t="2"]').dataset.k === 'water'`, 5*time.Second)
	bob.Wait(t, `document.querySelector('#town-earlier [data-here]').textContent.includes('flooded C1, where a tree was')`, 5*time.Second)
	bob.Wait(t, fmt.Sprintf(`document.querySelector('.town-slider').value === '%d'`, head+3), 5*time.Second)

	// Bob builds a house on the water: refused, and only Bob hears why.
	townClick(t, bob, 2)
	bob.Wait(t, `document.querySelector('#town-outcome').textContent.includes('Only a road goes on water')`, 5*time.Second)
	var heard string
	ada.Eval(t, `document.querySelector('#town-outcome').textContent`, &heard)
	if heard != "" {
		t.Errorf("Ada hears %q", heard)
	}

	for name, p := range map[string]*browser.Page{"Ada": ada, "Bob": bob} {
		if errs := p.Errors(); len(errs) > 0 {
			t.Errorf("%s: %d errors:\n%v", name, len(errs), errs)
		}
	}
}

// townNext reads events until each of wants has come, in any order: the
// views of one tick reach a stream in the order it watches them. It
// returns the events read.
func townNext(t *testing.T, events <-chan string, wants ...string) string {
	t.Helper()
	var read strings.Builder
	timeout := time.After(5 * time.Second)
	for left := wants; len(left) > 0; {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("the stream ended before %q", left)
			}
			read.WriteString(event)
			left = slices.DeleteFunc(left, func(w string) bool { return strings.Contains(event, w) })
		case <-timeout:
			t.Fatalf("no events with %q in 5s", left)
		}
	}
	return read.String()
}

// townClick clicks tile i with the mouse.
func townClick(t *testing.T, p *browser.Page, i int) {
	t.Helper()
	p.Eval(t, `document.querySelector('.town-board').scrollIntoView({block: 'center', behavior: 'instant'})`, nil)
	r := boardRect(t, p, fmt.Sprintf(`#town-map [data-t="%d"]`, i))
	x, y := r.X+r.W/2, r.Y+r.H/2
	for _, typ := range []string{"mousePressed", "mouseReleased"} {
		p.Call(t, "Input.dispatchMouseEvent", map[string]any{"type": typ, "x": x, "y": y, "button": "left", "buttons": 1, "clickCount": 1})
	}
}

// townTool picks a tool from the palette.
func townTool(t *testing.T, p *browser.Page, kind string) {
	t.Helper()
	p.Eval(t, `document.querySelector('.town-tool input[value="`+kind+`"]').click()`, nil)
}

// townDrag drags the slider's thumb from the end to a fraction of the
// track, in small steps, with the mouse.
func townDrag(t *testing.T, p *browser.Page, to float64) {
	t.Helper()
	p.Eval(t, `document.querySelector('.town-slider').scrollIntoView({block: 'center', behavior: 'instant'})`, nil)
	r := boardRect(t, p, ".town-slider")
	y, from, end := r.Y+r.H/2, r.X+r.W-6, r.X+2+(r.W-4)*to
	mouse := func(typ string, x float64) {
		p.Call(t, "Input.dispatchMouseEvent", map[string]any{"type": typ, "x": x, "y": y, "button": "left", "buttons": 1, "clickCount": 1})
	}
	mouse("mousePressed", from)
	for i := 1; i <= 20; i++ {
		mouse("mouseMoved", from+(end-from)*float64(i)/20)
		time.Sleep(16 * time.Millisecond)
	}
	mouse("mouseReleased", end)
}

// townPress sends a key to the focused element.
func townPress(t *testing.T, p *browser.Page, key string) {
	t.Helper()
	boardPress(t, p, key)
}

// TestTownShots writes screenshots of the town: in the light theme, now
// and back in the past, its badge saying so, and on a phone.
//
//	LITH_SITE_SHOTS=/tmp/shots LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestTownShots
func TestTownShots(t *testing.T) {
	dir := os.Getenv("LITH_SITE_SHOTS")
	if dir == "" {
		t.Skip("set LITH_SITE_SHOTS to a directory for the screenshots")
	}
	_, base := apptest.Start(t)
	b := browser.Start(t)
	p := b.NewPage(t, 1440, 1000)
	p.Navigate(t, base+"/")
	p.Eval(t, "document.cookie = 'lith-theme=light; Path=/'", nil)
	p.Navigate(t, base+"/demos/town")
	p.Wait(t, `document.querySelector('.live-badge.is-live') && document.querySelector('#town-map [data-t="44"]').dataset.k === 'bridge'`, 10*time.Second)
	p.Eval(t, "document.fonts.ready.then(() => true)", nil)
	p.Eval(t, `document.querySelector('.demo-stage').scrollIntoView({block: 'start', behavior: 'instant'})`, nil)
	time.Sleep(300 * time.Millisecond)
	p.Screenshot(t, filepath.Join(dir, "town-live-light.png"), false)
	townDrag(t, p, 0.5)
	p.Wait(t, `document.querySelector('#town-map').dataset.at !== '0' && document.querySelector('.town-board').getAttribute('aria-busy') !== 'true'`, 5*time.Second)
	p.Wait(t, `document.querySelector('.live-badge.is-held')`, 2*time.Second)
	p.Eval(t, `document.querySelector('.demo-stage').scrollIntoView({block: 'start', behavior: 'instant'})`, nil)
	time.Sleep(500 * time.Millisecond)
	p.Screenshot(t, filepath.Join(dir, "town-past-light.png"), false)

	m := b.NewPage(t, 390, 844)
	m.Call(t, "Emulation.setDeviceMetricsOverride", map[string]any{"width": 390, "height": 844, "deviceScaleFactor": 2, "mobile": true})
	m.Navigate(t, base+"/demos/town")
	m.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)
	m.Eval(t, `document.querySelector('.demo-stage').scrollIntoView({block: 'start', behavior: 'instant'})`, nil)
	time.Sleep(300 * time.Millisecond)
	m.Screenshot(t, filepath.Join(dir, "town-mobile.png"), false)
	var overflow bool
	m.Eval(t, `document.documentElement.scrollWidth > document.documentElement.clientWidth`, &overflow)
	if overflow {
		t.Error("the town scrolls sideways on a phone")
	}
	for name, page := range map[string]*browser.Page{"desktop": p, "phone": m} {
		if errs := page.Errors(); len(errs) > 0 {
			t.Errorf("%s: %v", name, errs)
		}
	}
}
