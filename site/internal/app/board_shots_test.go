package app_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
)

// TestShotsBoard writes screenshots of the shared board: its page in both
// themes, in the smooth style and on a phone; a card dragged by the mouse,
// one picked up from the keyboard, a move waiting for its frame, a refused
// one; and the landing page's slot.
//
//	LITH_SITE_SHOTS=/tmp/shots LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestShotsBoard
func TestShotsBoard(t *testing.T) {
	dir := os.Getenv("LITH_SITE_SHOTS")
	if dir == "" {
		t.Skip("set LITH_SITE_SHOTS to a directory for the screenshots")
	}
	a, base := apptest.Start(t)
	b := browser.Start(t)

	// Two people, a few moves: the numbers and the faces of the movers.
	ada := boardOpen(t, b, base, 1440, 1000, "")
	bob := boardOpen(t, b, base, 1440, 1000, "")
	boardKeyMove(t, ada, 4, "ArrowRight", "ArrowUp", "ArrowUp")
	boardWaitCard(t, bob, 4, 1)
	time.Sleep(700 * time.Millisecond)
	boardKeyMove(t, bob, 11, "ArrowLeft")
	boardWaitCard(t, ada, 11, 1)
	time.Sleep(1500 * time.Millisecond)

	stage := func(p *browser.Page, name string) {
		t.Helper()
		boardSettled(t, p)
		p.ShotElement(t, ".demo-stage", filepath.Join(dir, name+".png"), 1)
	}
	stage(ada, "board")

	// The mouse, halfway: the ghost, the lane it would go to, the marker.
	ada.Eval(t, `document.querySelector('.bd__board').scrollIntoView({block: 'center', behavior: 'instant'})`, nil)
	s := boardRect(t, ada, "#board-card-12")
	d := boardRect(t, ada, "#board-card-2")
	mouse := func(typ string, x, y float64) {
		ada.Call(t, "Input.dispatchMouseEvent", map[string]any{"type": typ, "x": x, "y": y, "button": "left", "buttons": 1, "clickCount": 1})
	}
	mouse("mousePressed", s.X+s.W/2, s.Y+s.H/2)
	for i := 1; i <= 10; i++ {
		f := float64(i) / 10
		mouse("mouseMoved", s.X+s.W/2+(d.X+d.W*0.6-s.X-s.W/2)*f, s.Y+s.H/2+(d.Y+d.H*0.35-s.Y-s.H/2)*f)
		time.Sleep(16 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)
	stage(ada, "board-drag")
	ada.Call(t, "Input.dispatchKeyEvent", map[string]any{"type": "rawKeyDown", "key": "Escape", "code": "Escape", "windowsVirtualKeyCode": 27})
	mouse("mouseReleased", d.X, d.Y)
	time.Sleep(300 * time.Millisecond)

	// The keyboard: a card picked up, the place it would go.
	boardFocus(t, bob, 7)
	boardPress(t, bob, " ")
	boardPress(t, bob, "ArrowRight")
	boardPress(t, bob, "ArrowUp")
	time.Sleep(400 * time.Millisecond)
	stage(bob, "board-keys")
	boardPress(t, bob, "Escape")

	// A move that waits: the writer is busy for a moment (Exclusive holds
	// it), and the card stays where it was, marked pending, until its
	// frame comes.
	held, release := make(chan struct{}), make(chan struct{})
	go a.Writer().Exclusive(context.Background(), func(context.Context, *sql.Conn) error {
		close(held)
		<-release
		return nil
	})
	<-held
	boardFocus(t, ada, 9)
	boardPress(t, ada, " ")
	boardPress(t, ada, "ArrowLeft")
	boardPress(t, ada, " ")
	time.Sleep(600 * time.Millisecond)
	stage(ada, "board-pending")
	close(release)
	boardWaitCard(t, bob, 9, 1)
	time.Sleep(time.Second)

	// A refusal: Bob holds a card that Ada moves first.
	boardFocus(t, bob, 1)
	boardPress(t, bob, " ")
	boardKeyMove(t, ada, 1, "ArrowRight")
	boardWaitCard(t, bob, 1, 1)
	boardPress(t, bob, "ArrowRight")
	boardPress(t, bob, "ArrowRight")
	boardPress(t, bob, " ")
	bob.Wait(t, `document.querySelector('#board-outcome.is-refused')`, 5*time.Second)
	time.Sleep(150 * time.Millisecond)
	stage(bob, "board-refused")

	for _, v := range []struct {
		name, cookie string
		w, h         int
		full         bool
	}{
		{"board-light", "lith-theme=light", 1440, 1000, false},
		{"board-smooth", "lith-style=smooth", 1440, 1000, false},
		{"board-mobile", "", 390, 844, true},
		{"board-mobile-light", "lith-theme=light", 390, 844, true},
	} {
		p := boardOpen(t, b, base, v.w, v.h, v.cookie)
		time.Sleep(1200 * time.Millisecond)
		if v.full {
			p.Call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "reduce"}}})
			p.Screenshot(t, filepath.Join(dir, v.name+".png"), true)
		} else {
			stage(p, v.name)
		}
		p.Close(t)
	}

	for _, v := range []struct {
		name, cookie string
		w, h         int
	}{
		{"board-slot", "", 1440, 1000},
		{"board-slot-light", "lith-theme=light", 1440, 1000},
		{"board-slot-mobile", "", 390, 844},
	} {
		p := b.NewPage(t, v.w, v.h)
		if v.cookie != "" {
			p.Navigate(t, base+"/")
			p.Eval(t, "document.cookie = '"+v.cookie+"; Path=/'", nil)
		}
		p.Navigate(t, base+"/")
		p.Eval(t, "document.fonts.ready.then(() => true)", nil)
		p.Wait(t, `document.querySelector('[data-demo="board"] .is-live')`, 10*time.Second)
		p.Eval(t, `document.querySelector('#ticks').scrollIntoView({block: 'center', behavior: 'instant'})`, nil)
		time.Sleep(1500 * time.Millisecond) // the scroll-driven reveal
		p.ShotElement(t, "#ticks", filepath.Join(dir, v.name+".png"), 1)
		p.Close(t)
	}

	for name, p := range map[string]*browser.Page{"Ada": ada, "Bob": bob} {
		if errs := p.Errors(); len(errs) > 0 {
			t.Errorf("%s: %v", name, errs)
		}
	}
}

// TestTransitionShotsBoard writes a sequence of screenshots of a move by
// another tab, at a tenth of the speed: the card lifts out of its lane,
// glides to the other and lands with a glow, while the others close the
// gap and make room. Then a card arriving in the landing page's miniature.
//
//	LITH_SITE_SHOTS=/tmp/shots LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestTransitionShotsBoard
func TestTransitionShotsBoard(t *testing.T) {
	dir := os.Getenv("LITH_SITE_SHOTS")
	if dir == "" {
		t.Skip("set LITH_SITE_SHOTS to a directory for the screenshots")
	}
	_, base := apptest.Start(t)
	b := browser.Start(t)
	mover := boardOpen(t, b, base, 1280, 900, "")
	watcher := boardOpen(t, b, base, 1280, 900, "")
	watcher.Call(t, "Animation.enable", nil)
	watcher.Eval(t, `document.querySelector('.bd__board').scrollIntoView({block: 'center', behavior: 'instant'})`, nil)
	time.Sleep(500 * time.Millisecond)
	watcher.Call(t, "Animation.setPlaybackRate", map[string]any{"playbackRate": 0.1})
	clip := boardRect(t, watcher, ".bd__board")
	boardKeyMove(t, mover, 2, "ArrowRight", "ArrowRight", "ArrowUp", "ArrowUp", "ArrowUp")
	for i := range 7 {
		time.Sleep(900 * time.Millisecond)
		watcher.Call(t, "Animation.setPlaybackRate", map[string]any{"playbackRate": 0.1})
		boardShotClip(t, watcher, clip, filepath.Join(dir, fmt.Sprintf("vt-board-%d.png", i)))
	}
	watcher.Call(t, "Animation.setPlaybackRate", map[string]any{"playbackRate": 1})

	home := b.NewPage(t, 1440, 1000)
	home.Call(t, "Animation.enable", nil)
	home.Navigate(t, base+"/")
	home.Wait(t, `document.querySelector('[data-demo="board"] .is-live')`, 10*time.Second)
	home.Eval(t, `document.querySelector('#ticks').scrollIntoView({block: 'center', behavior: 'instant'})`, nil)
	time.Sleep(1500 * time.Millisecond)
	home.Call(t, "Animation.setPlaybackRate", map[string]any{"playbackRate": 0.2})
	mini := boardRect(t, home, "#board-mini")
	boardKeyMove(t, mover, 6, "ArrowLeft", "ArrowUp")
	for i := range 4 {
		time.Sleep(500 * time.Millisecond)
		boardShotClip(t, home, mini, filepath.Join(dir, fmt.Sprintf("vt-board-mini-%d.png", i)))
	}
	for name, p := range map[string]*browser.Page{"the mover": mover, "the watcher": watcher, "the landing page": home} {
		if errs := p.Errors(); len(errs) > 0 {
			t.Errorf("%s: %v", name, errs)
		}
	}
}

// boardSettled waits until no view transition runs: a screenshot taken during
// one shows its snapshots, not the page.
func boardSettled(t *testing.T, p *browser.Page) {
	t.Helper()
	p.Wait(t, `!document.documentElement.matches(':active-view-transition')`, 5*time.Second)
}

// boardShotClip writes a PNG of a part of the viewport, without scrolling: a
// view transition's snapshots stay where they were captured.
func boardShotClip(t *testing.T, p *browser.Page, r boardBox, path string) {
	t.Helper()
	res := p.Call(t, "Page.captureScreenshot", map[string]any{"format": "png", "clip": map[string]any{"x": r.X, "y": r.Y, "width": r.W, "height": r.H, "scale": 1}})
	var shot struct{ Data string }
	if err := json.Unmarshal(res, &shot); err != nil {
		t.Fatal(err)
	}
	png, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, png, 0o644); err != nil {
		t.Fatal(err)
	}
}

// boardOpen opens the board's page, its stream live and its fonts in.
func boardOpen(t *testing.T, b *browser.Browser, base string, w, h int, cookie string) *browser.Page {
	t.Helper()
	p := b.NewPage(t, w, h)
	if cookie != "" {
		p.Navigate(t, base+"/")
		p.Eval(t, "document.cookie = '"+cookie+"; Path=/'", nil)
	}
	p.Navigate(t, base+"/demos/board")
	p.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)
	p.Eval(t, "document.fonts.ready.then(() => true)", nil)
	return p
}

// boardKeyMove picks a card up with the keyboard, presses the keys, and drops
// it.
func boardKeyMove(t *testing.T, p *browser.Page, card int, keys ...string) {
	t.Helper()
	boardFocus(t, p, card)
	boardPress(t, p, " ")
	for _, k := range keys {
		boardPress(t, p, k)
	}
	boardPress(t, p, " ")
}

// boardWaitCard waits until the page shows the card in the lane.
func boardWaitCard(t *testing.T, p *browser.Page, card, lane int) {
	t.Helper()
	p.Wait(t, fmt.Sprintf(`document.querySelector('#board-card-%d')?.closest('[data-lane]')?.dataset.lane === '%d'`, card, lane), 5*time.Second)
}
