package app_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/app"
	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
	"github.com/zweiundeins/lith/site/internal/demos/checkboxes"
)

// TestShotsCheckboxes writes screenshots of the million checkboxes: both
// themes, both styles, a phone, a jump far into the grid, and a box that
// waits for its frame.
//
//	LITH_SITE_SHOTS=/tmp/shots LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestShotsCheckboxes
func TestShotsCheckboxes(t *testing.T) {
	dir := os.Getenv("LITH_SITE_SHOTS")
	if dir == "" {
		t.Skip("set LITH_SITE_SHOTS to a directory for the screenshots")
	}
	a, base := apptest.Start(t)
	b := browser.Start(t)
	cbSeedBoxes(t, a)

	shot := func(name string, w, h int, setup, then string) {
		p := b.NewPage(t, w, h)
		defer p.Close(t)
		if setup != "" {
			p.Navigate(t, base+"/")
			p.Eval(t, setup, nil)
		}
		p.Navigate(t, base+"/demos/checkboxes")
		p.Eval(t, "document.fonts.ready.then(() => true)", nil)
		p.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)
		if then != "" {
			p.Eval(t, then, nil)
		}
		time.Sleep(1200 * time.Millisecond)
		p.Screenshot(t, filepath.Join(dir, name+".png"), false)
		if errs := p.Errors(); len(errs) > 0 {
			t.Errorf("%s: %v", name, errs)
		}
	}
	scroll := `document.querySelector('.demo-stage').scrollIntoView({block: 'start'})`
	shot("checkboxes", 1440, 900, "", "")
	shot("checkboxes-grid", 1440, 900, "", scroll)
	shot("checkboxes-light", 1440, 900, "document.cookie = 'lith-theme=light; Path=/'", scroll)
	shot("checkboxes-smooth", 1440, 900, "document.cookie = 'lith-style=smooth; Path=/'", scroll)
	shot("checkboxes-mobile", 390, 844, "", scroll)
	shot("checkboxes-jump", 1440, 900, "", scroll+`; (() => {
		const input = document.querySelector('.cb-jump input')
		input.value = '500000'
		input.dispatchEvent(new Event('input', {bubbles: true}))
		document.querySelector('.cb-jump button').click()
	})()`)

	// The whole page, "How it works" open.
	full := b.NewPage(t, 1440, 900)
	full.Call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "reduce"}}})
	full.Navigate(t, base+"/demos/checkboxes")
	full.Eval(t, "document.fonts.ready.then(() => true)", nil)
	full.Eval(t, "document.querySelector('.howto').open = true", nil)
	time.Sleep(600 * time.Millisecond)
	full.Screenshot(t, filepath.Join(dir, "checkboxes-full.png"), true)
	full.ShotElement(t, ".demo-head__art", filepath.Join(dir, "checkboxes-detail-art.png"), 2)

	// Two tabs on the same tiles, and boxes that wait for their frame:
	// the writer is held while Ada clicks.
	ada := b.NewPage(t, 1440, 900)
	ada.Navigate(t, base+"/demos/checkboxes")
	ada.Wait(t, `document.querySelector('.live-badge.is-live') && customElements.get('sb-virtual-scroll')`, 10*time.Second)
	ada.Eval(t, scroll, nil)
	ada.Wait(t, `document.querySelector('#checkboxes-tile-0 .cb-tile__eyes')`, 5*time.Second)
	held, release := make(chan struct{}), make(chan struct{})
	go a.Writer().Exclusive(context.Background(), func(context.Context, *sql.Conn) error {
		close(held)
		<-release
		return nil
	})
	<-held
	ada.Eval(t, `[3, 20, 37, 54, 71].forEach((i) => document.querySelector('#checkboxes-tile-0 .cb-boxes').children[i].click())`, nil)
	ada.Wait(t, `document.querySelector('#checkboxes-tile-0 .cb-boxes').children[71].indeterminate`, 3*time.Second)
	ada.ShotElement(t, "#checkboxes-slot-0", filepath.Join(dir, "checkboxes-detail-pending.png"), 2)
	ada.Screenshot(t, filepath.Join(dir, "checkboxes-watching.png"), false)
	close(release)
	ada.Wait(t, `document.querySelector('#checkboxes-tile-0 .cb-boxes').children[71].checked`, 5*time.Second)
	time.Sleep(700 * time.Millisecond)
	ada.ShotElement(t, "#checkboxes-slot-0", filepath.Join(dir, "checkboxes-detail-done.png"), 2)
	ada.ShotElement(t, ".cb-stats", filepath.Join(dir, "checkboxes-detail-stats.png"), 2)
	if errs := ada.Errors(); len(errs) > 0 {
		t.Errorf("ada: %v", errs)
	}
}

// cbSeedBoxes checks a pattern of boxes in the first tiles and around box
// 500,000, as if people had been there.
func cbSeedBoxes(t *testing.T, a *app.App) {
	t.Helper()
	at := time.Now()
	for _, from := range []int{0, 499_712} {
		for i := range 16 * 256 {
			x, y, c := i%16, i/16%16, i/256
			if (x+y+c)%7 == 0 || (x*y+c)%11 == 3 {
				if err := a.Writer().Do(context.Background(), checkboxes.SetBox{Box: from + i, Checked: true, At: at}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}
