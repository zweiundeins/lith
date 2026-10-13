package app_test

import (
	"context"
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
	seedBoxes(t, a)

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
}

// seedBoxes checks a pattern of boxes in the first tiles and around box
// 500,000, as if people had been there.
func seedBoxes(t *testing.T, a *app.App) {
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
