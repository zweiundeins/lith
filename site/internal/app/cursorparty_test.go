package app_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
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
	cursorparty "github.com/zweiundeins/lith/site/internal/demos/cursor-party"
)

// TestCursorPartyStream posts moves, which answer 204 at once, and reads
// them from the stream: the first move adds a cursor to the layer, later
// ones move it, an older one changes nothing, and the counter counts the
// tab and the moves.
func TestCursorPartyStream(t *testing.T) {
	_, base := apptest.Start(t)
	c := client(t)
	get(t, c, base+"/demos/cursor-party") // the session's cookie
	events := listen(t, c, base+"/demos/cursor-party/stream?tab=tab1")
	next(t, events, `id="cursor-party-cursors"`)
	next(t, events, `--cursor-party-here:1;`)

	id, name, _ := cursorparty.Identity("tab2")
	move := func(seq int, x, y float64) int {
		return post(t, c, base+"/cmd/cursor-party/move", fmt.Sprintf(`{"tab":"tab2","x":%g,"y":%g,"seq":%d}`, x, y, seq))
	}
	if code := move(1, 0.25, 0.5); code != http.StatusNoContent {
		t.Fatalf("move: %d", code)
	}
	frame := next(t, events, `id="cursor-party-`+id+`"`)
	if !strings.Contains(frame, `style="--x:25.0;--y:50.0"`) || !strings.Contains(frame, "<b>"+name+"</b>") {
		t.Errorf("the first move:\n%s", frame)
	}
	move(3, 0.75, 0.125)
	next(t, events, `style="--x:75.0;--y:12.5"`)
	move(2, 0.5, 0.5) // overtaken by move 3: nothing changes
	move(4, 0.8, 0.2)
	if frame := next(t, events, `id="cursor-party-`+id+`"`); !strings.Contains(frame, `--x:80.0;--y:20.0`) {
		t.Errorf("after an older move and a newer one:\n%s", frame)
	}
	if code := move(5, 1.5, 0.5); code != http.StatusBadRequest {
		t.Errorf("a move off the floor: %d", code)
	}
	next(t, events, `--cursor-party-rate:`)
}

// TestCursorParty is the party in Chromium: Ada and Bob open the demo,
// Cleo the landing page. Ada's pointer crosses the floor; Bob sees her
// cursor arrive, named and coloured, and settle where her pointer rests,
// and so does the hero behind Cleo's title, while Ada's own page keeps
// her cursor hidden. Then Bob moves, and Ada sees him; Dee drags a finger
// across a phone's floor, and Bob sees her; Ada's cursor, at rest, fades
// and leaves. With reduced motion the trail is gone.
//
//	LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestCursorParty
func TestCursorParty(t *testing.T) {
	a, base := apptest.Start(t)
	b := browser.Start(t)
	ada := b.NewPage(t, 1280, 900)
	ada.Navigate(t, base+"/demos/cursor-party")
	bob := b.NewPage(t, 1280, 900)
	bob.Navigate(t, base+"/demos/cursor-party")
	cleo := b.NewPage(t, 1280, 900)
	cleo.Navigate(t, base+"/")
	for _, p := range []*browser.Page{ada, bob} {
		p.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)
	}
	cleo.Wait(t, `document.querySelector('.cp-badge.is-live')`, 10*time.Second)
	waitFor(t, "three tabs at the party", func() bool { return a.Hub().Count("cursor-party:count") == 3 })
	bob.Wait(t, `getComputedStyle(document.querySelector('#cursor-party-count')).getPropertyValue('--cursor-party-here') === '3'`, 5*time.Second)

	// Ada's pointer crosses the floor from left to right, about ten
	// moves a second, and rests at 70%, 40%: by the nearest anchor of the
	// hero, which a demo page puts where the desktop hero has it.
	glide(t, ada, ".cp__floor", [][2]float64{{0.2, 0.6}, {0.7, 0.4}}, 12)
	adaID := tabCursor(t, ada)
	sel := `document.querySelector('#cursor-party-` + adaID + `')`
	bob.Wait(t, sel, 5*time.Second)
	bob.Wait(t, cpTipNear(sel, 0.7, 0.4), 5*time.Second)
	var seen struct {
		Display, Name string
		Floor         [4]float64
	}
	bob.Eval(t, `(() => {
		const c = `+sel+`, f = document.querySelector('.cp__floor').getBoundingClientRect()
		return {display: getComputedStyle(c).display, name: c.textContent, floor: [f.left, f.top, f.width, f.height]}
	})()`, &seen)
	if seen.Display == "none" || !strings.Contains(seen.Name, " ") {
		t.Errorf("Bob sees Ada's cursor as %+v", seen)
	}
	// The arrow's tip settles on Ada's pointer, in Bob's floor: the tag's
	// corner and the arrow's place on it.
	time.Sleep(400 * time.Millisecond)
	var tip [2]float64
	bob.Eval(t, `(() => {
		const b = `+sel+`.querySelector('b'), r = b.getBoundingClientRect(), a = getComputedStyle(b, '::before')
		return [r.left + parseFloat(a.left), r.top + parseFloat(a.top)]
	})()`, &tip)
	wantX, wantY := seen.Floor[0]+0.7*seen.Floor[2], seen.Floor[1]+0.4*seen.Floor[3]
	if math.Abs(tip[0]-wantX) > 2 || math.Abs(tip[1]-wantY) > 2 {
		t.Errorf("Ada's arrow points at %v in Bob's floor, want %.0f, %.0f", tip, wantX, wantY)
	}
	// Ada's own page holds her cursor and never shows it.
	ada.Wait(t, sel, 5*time.Second)
	var display string
	ada.Eval(t, `getComputedStyle(`+sel+`).display`, &display)
	if display != "none" {
		t.Errorf("Ada sees her own cursor drawn back: display %q", display)
	}
	// The hero behind Cleo's title shows it.
	cleo.Wait(t, sel+` && getComputedStyle(`+sel+`).display !== 'none' && `+sel+`.dataset.a`, 5*time.Second)
	bob.Wait(t, `getComputedStyle(document.querySelector('#cursor-party-count')).getPropertyValue('--cursor-party-rate') !== '0'`, 5*time.Second)
	// The counter's sparkline (Starbase's sb-sparkline) holds the last 30
	// seconds, Ada's moves among them, on both pages.
	for _, p := range []*browser.Page{bob, cleo} {
		p.Wait(t, `(s => customElements.get('sb-sparkline') && s.data.length === 30 && Math.max(...s.data) > 0)(document.querySelector('#cursor-party-count sb-sparkline'))`, 5*time.Second)
	}

	// Bob moves; Ada sees him, and only him.
	glide(t, bob, ".cp__floor", [][2]float64{{0.9, 0.9}, {0.3, 0.2}}, 8)
	bobID := tabCursor(t, bob)
	ada.Wait(t, cpTipNear(`document.querySelector('#cursor-party-`+bobID+`')`, 0.3, 0.2), 5*time.Second)
	var shown int
	ada.Eval(t, `[...document.querySelectorAll('#cursor-party-cursors > i')].filter(c => getComputedStyle(c).display !== 'none').length`, &shown)
	if shown != 1 {
		t.Errorf("Ada sees %d cursors, want Bob's", shown)
	}

	// A finger that drags across the floor of a phone moves its cursor
	// too.
	dee := b.NewPage(t, 390, 844)
	dee.Call(t, "Emulation.setTouchEmulationEnabled", map[string]any{"enabled": true, "maxTouchPoints": 1})
	dee.Navigate(t, base+"/demos/cursor-party")
	dee.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)
	drag(t, dee, ".cp__floor", [2]float64{0.2, 0.8}, [2]float64{0.6, 0.3}, 10)
	deeID := tabCursor(t, dee)
	bob.Wait(t, cpTipNear(`document.querySelector('#cursor-party-`+deeID+`')`, 0.6, 0.3), 5*time.Second)

	// Reduced motion: no trail, no easing. And in the smooth style the
	// arrow has straight edges.
	t.Run("reduced motion", func(t *testing.T) {
		browser.SkipFirefox(t, "the Firefox driver cannot emulate prefers-reduced-motion")
		still := b.NewPage(t, 1280, 900)
		still.Call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "reduce"}}})
		still.Navigate(t, base+"/")
		still.Eval(t, `document.cookie = 'lith-style=smooth; Path=/'`, nil)
		still.Navigate(t, base+"/demos/cursor-party")
		deeSel := `document.querySelector('#cursor-party-` + deeID + `')`
		still.Wait(t, deeSel, 5*time.Second)
		var motion struct {
			Trail, Arrow string
			Ease         float64 // the tag's longest transition, in seconds
		}
		still.Eval(t, `(() => {
			const c = `+deeSel+`, b = c.querySelector('b'), d = getComputedStyle(b).transitionDuration.split(',').map(parseFloat)
			return {trail: getComputedStyle(c, '::before').display, arrow: getComputedStyle(b, '::before').maskImage, ease: Math.max(...d)}
		})()`, &motion)
		if motion.Trail != "none" || motion.Ease > 0.001 || !strings.Contains(motion.Arrow, "M0 0L16 15.5") {
			t.Errorf("with reduced motion, smooth: %+v", motion)
		}
		if errs := still.Errors(); len(errs) > 0 {
			t.Errorf("the still page: %v", errs)
		}
	})

	// Ada has rested for a few seconds: her cursor fades on Bob's page,
	// then leaves it.
	bob.Wait(t, sel+`?.hasAttribute('data-idle')`, 5*time.Second)
	bob.Wait(t, `!`+sel, 5*time.Second)

	for name, p := range map[string]*browser.Page{"Ada": ada, "Bob": bob, "Cleo": cleo, "Dee": dee} {
		if errs := p.Errors(); len(errs) > 0 {
			t.Errorf("%s: %v", name, errs)
		}
	}
}

// cpTipNear is an expression that is true once the arrow of the cursor
// (el, an expression) points within 2 pixels of x, y, fractions of the
// page's floor: a demo page puts the hero's anchors where the desktop hero
// has them, as fractions of its floor, so every demo page agrees.
func cpTipNear(el string, x, y float64) string {
	return fmt.Sprintf(`((c) => {
		if (!c) return false
		const b = c.querySelector('b'), r = b.getBoundingClientRect(), a = getComputedStyle(b, '::before'), f = document.querySelector('.cp__floor').getBoundingClientRect()
		return Math.abs(r.left + parseFloat(a.left) - f.left - %g * f.width) < 2 && Math.abs(r.top + parseFloat(a.top) - f.top - %g * f.height) < 2
	})(%s)`, x, y, el)
}

// glide moves the mouse over the element in steps from one point to the
// next (fractions of its box), steps a leg, ten a second.
func glide(t *testing.T, p *browser.Page, selector string, points [][2]float64, steps int) {
	t.Helper()
	var r struct{ X, Y, W, H float64 }
	p.Eval(t, `(() => { const e = document.querySelector(`+"`"+selector+"`"+`); e.scrollIntoView({block: 'center'}); const r = e.getBoundingClientRect(); return {x: r.left, y: r.top, w: r.width, h: r.height} })()`, &r)
	for i := 1; i < len(points); i++ {
		from, to := points[i-1], points[i]
		for s := 0; s <= steps; s++ {
			f := float64(s) / float64(steps)
			x := r.X + r.W*(from[0]+(to[0]-from[0])*f)
			y := r.Y + r.H*(from[1]+(to[1]-from[1])*f)
			p.Call(t, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": x, "y": y})
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// drag moves a finger over the element from one point to another
// (fractions of its box) in steps, ten a second, and lifts it.
func drag(t *testing.T, p *browser.Page, selector string, from, to [2]float64, steps int) {
	t.Helper()
	var r struct{ X, Y, W, H float64 }
	p.Eval(t, `(() => { const e = document.querySelector(`+"`"+selector+"`"+`); e.scrollIntoView({block: 'center'}); const r = e.getBoundingClientRect(); return {x: r.left, y: r.top, w: r.width, h: r.height} })()`, &r)
	at := func(f float64) []map[string]any {
		return []map[string]any{{"x": r.X + r.W*(from[0]+(to[0]-from[0])*f), "y": r.Y + r.H*(from[1]+(to[1]-from[1])*f)}}
	}
	p.Call(t, "Input.dispatchTouchEvent", map[string]any{"type": "touchStart", "touchPoints": at(0)})
	for s := 1; s <= steps; s++ {
		time.Sleep(100 * time.Millisecond)
		p.Call(t, "Input.dispatchTouchEvent", map[string]any{"type": "touchMove", "touchPoints": at(float64(s) / float64(steps))})
	}
	p.Call(t, "Input.dispatchTouchEvent", map[string]any{"type": "touchEnd", "touchPoints": []map[string]any{}})
}

// tabCursor is the id of the page's own cursor, from its tab id.
func tabCursor(t *testing.T, p *browser.Page) string {
	t.Helper()
	var tab string
	p.Eval(t, `document.body.getAttribute('data-signals').match(/_tab: "([^"]+)"/)[1]`, &tab)
	id, _, _ := cursorparty.Identity(tab)
	return id
}

// BenchmarkCursorPartyMove is one move through the whole site, as each of
// the 750 moves a second of 50 pointers takes it: the edge (the request
// log, compression, the security headers, CSRF, the session), the rate
// limits, the handler and the writer's queue. Moves come from 2,000 tabs,
// each with a session and an address of its own, so no limit refuses
// one; the writer applies them meanwhile, on its own goroutine, and the
// loop waits for it every 1,000 moves (untimed), since nothing here
// limits the pace to a party's.
//
//	go test ./internal/app -run '^$' -bench CursorPartyMove
func BenchmarkCursorPartyMove(b *testing.B) {
	a, _ := apptest.Start(b)
	h := a.Handler()
	type tab struct {
		cookie, addr, id string
		seq              int
	}
	tabs := make([]tab, 2000)
	for i := range tabs {
		sid := make([]byte, 32)
		rand.Read(sid)
		tabs[i] = tab{cookie: "sid=" + base64.RawURLEncoding.EncodeToString(sid), addr: fmt.Sprintf("10.%d.%d.%d", i>>16, i>>8&255, i&255), id: fmt.Sprintf("bench-%d", i)}
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		if i%1000 == 999 {
			b.StopTimer()
			a.Writer().Flush(context.Background())
			b.StartTimer()
		}
		t := &tabs[i%len(tabs)]
		i++
		t.seq++
		body := fmt.Sprintf(`{"tab":%q,"x":0.%03d,"y":0.5,"seq":%d}`, t.id, i%1000, t.seq)
		r := httptest.NewRequest("POST", "/cmd/cursor-party/move", strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:4711" // the proxy, which names the client
		r.Header.Set("X-Forwarded-For", t.addr)
		r.Header.Set("Cookie", t.cookie)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Datastar-Request", "true")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			b.Fatalf("move %d: %d %s", i, w.Code, w.Body)
		}
	}
	b.StopTimer()
	if err := a.Writer().Flush(context.Background()); err != nil {
		b.Fatal(err)
	}
}

// TestShotsCursorParty writes screenshots of the party, with a crowd of
// cursors that keep moving, on the landing page and the demo's page, in
// both themes, smooth, and on a phone:
//
//	LITH_SITE_SHOTS=/tmp/shots LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestShotsCursorParty
func TestShotsCursorParty(t *testing.T) {
	dir := os.Getenv("LITH_SITE_SHOTS")
	if dir == "" {
		t.Skip("set LITH_SITE_SHOTS to a directory for the screenshots")
	}
	a, base := apptest.Start(t)
	stop := crowd(t, a, 9)
	defer stop()
	b := browser.Start(t)
	shot := func(name, path string, w, h int, cookies string, wait time.Duration) {
		p := b.NewPage(t, w, h)
		defer p.Close(t)
		if cookies != "" {
			p.Navigate(t, base+"/")
			for _, c := range strings.Split(cookies, "; ") {
				p.Eval(t, "document.cookie = '"+c+"; Path=/'", nil)
			}
		}
		p.Navigate(t, base+path)
		p.Eval(t, "document.fonts.ready.then(() => true)", nil)
		p.Wait(t, `document.querySelectorAll('#cursor-party-cursors > i').length >= 5`, 10*time.Second)
		time.Sleep(wait)
		p.Screenshot(t, filepath.Join(dir, name+".png"), false)
		if errs := p.Errors(); len(errs) > 0 {
			t.Errorf("%s: %v", name, errs)
		}
	}
	shot("party-landing", "/", 1440, 900, "", 1500*time.Millisecond)
	shot("party-landing-light", "/", 1440, 900, "lith-theme=light", 1500*time.Millisecond)
	shot("party-landing-smooth", "/", 1440, 900, "lith-style=smooth", 1500*time.Millisecond)
	shot("party-demo", "/demos/cursor-party", 1440, 900, "", 1500*time.Millisecond)
	shot("party-demo-light", "/demos/cursor-party", 1440, 900, "lith-theme=light", 1500*time.Millisecond)
	shot("party-demo-smooth", "/demos/cursor-party", 1440, 900, "lith-style=smooth; lith-theme=light", 1500*time.Millisecond)
	shot("party-mobile-landing", "/", 390, 844, "", 1500*time.Millisecond)
	shot("party-mobile-demo", "/demos/cursor-party", 390, 844, "", 1500*time.Millisecond)

	// The whole page with its notes open; reduced motion, since the
	// scroll-driven reveals show nothing below the fold in one picture.
	full := b.NewPage(t, 1440, 900)
	full.Call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "reduce"}}})
	full.Navigate(t, base+"/demos/cursor-party")
	full.Eval(t, "document.fonts.ready.then(() => true)", nil)
	full.Eval(t, "document.querySelector('.howto').open = true", nil)
	time.Sleep(time.Second)
	full.Screenshot(t, filepath.Join(dir, "party-demo-full.png"), true)

	p := b.NewPage(t, 1440, 900)
	p.Navigate(t, base+"/demos/cursor-party")
	p.Eval(t, "document.fonts.ready.then(() => true)", nil)
	p.Wait(t, `document.querySelectorAll('#cursor-party-cursors > i').length >= 5`, 10*time.Second)
	time.Sleep(time.Second)
	for i := range 3 {
		p.ShotElement(t, ".cp__floor", filepath.Join(dir, fmt.Sprintf("party-floor-%d.png", i)), 1.5)
		time.Sleep(170 * time.Millisecond)
	}
}

// crowd moves n cursors over the party along curves of their own, 15
// moves a second each, until stop is called.
func crowd(t *testing.T, a *app.App, n int) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	start := time.Now()
	for i := range n {
		v := demo.Viewer{Session: fmt.Sprintf("crowd-session-%d", i), Tab: fmt.Sprintf("crowd-tab-%d", i)}
		wg.Go(func() {
			tick := time.NewTicker(cursorparty.Throttle)
			defer tick.Stop()
			for seq := int64(1); ; seq++ {
				s := time.Since(start).Seconds()*(0.25+0.05*float64(i)) + float64(i)
				x := 0.5 + 0.42*math.Sin(s*1.3+float64(i))*math.Cos(s*0.4)
				y := 0.5 + 0.4*math.Sin(s*0.9+float64(i)*2)
				a.Writer().Send(cursorparty.Move{Viewer: v, X: x, Y: y, Seq: seq, At: time.Now()})
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
			}
		})
	}
	return func() { cancel(); wg.Wait() }
}
