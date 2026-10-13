package app_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
)

// TestShots writes screenshots of the site's pages to look at:
//
//	LITH_SITE_SHOTS=/tmp/shots LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestShots
func TestShots(t *testing.T) {
	dir := os.Getenv("LITH_SITE_SHOTS")
	if dir == "" {
		t.Skip("set LITH_SITE_SHOTS to a directory for the screenshots")
	}
	_, base := apptest.Start(t)
	b := browser.Start(t)
	shot := func(name, path string, w, h int, full bool, setup string) {
		p := b.NewPage(t, w, h)
		defer p.Close(t)
		if full {
			// The scroll-driven reveals show nothing below the fold in one
			// picture of the whole page; reduced motion turns them off.
			p.Call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "reduce"}}})
		}
		if setup != "" {
			p.Navigate(t, base+"/")
			p.Eval(t, setup, nil)
		}
		p.Navigate(t, base+path)
		p.Eval(t, "document.fonts.ready.then(() => true)", nil)
		time.Sleep(900 * time.Millisecond) // the first paint's animations
		p.Screenshot(t, filepath.Join(dir, name+".png"), full)
		if errs := p.Errors(); len(errs) > 0 && name != "notfound" {
			t.Errorf("%s: %v", name, errs)
		}
	}
	shot("landing", "/", 1440, 900, true, "")
	shot("landing-light", "/", 1440, 900, false, "document.cookie = 'lith-theme=light; Path=/'")
	shot("landing-smooth", "/", 1440, 900, false, "document.cookie = 'lith-style=smooth; Path=/'")
	shot("docs", "/docs/concepts/ticks", 1440, 900, false, "")
	shot("docs-light", "/docs/getting-started/first-view", 1440, 900, false, "document.cookie = 'lith-theme=light; Path=/'")
	shot("guestbook", "/demos/guestbook", 1440, 900, true, "")
	shot("demos", "/demos", 1440, 900, false, "")
	shot("mobile-landing", "/", 390, 844, false, "")
	shot("mobile-docs", "/docs/concepts", 390, 844, false, "")
	shot("notfound", "/nope", 1440, 900, false, "")
}

// TestShotsDetail writes close-ups of parts of the pages.
func TestShotsDetail(t *testing.T) {
	dir := os.Getenv("LITH_SITE_SHOTS")
	if dir == "" {
		t.Skip("set LITH_SITE_SHOTS to a directory for the screenshots")
	}
	_, base := apptest.Start(t)
	b := browser.Start(t)
	p := b.NewPage(t, 1440, 900)
	p.Call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "reduce"}}})
	p.Navigate(t, base+"/")
	p.Eval(t, "document.fonts.ready.then(() => true)", nil)
	for name, sel := range map[string]string{"hero": ".hero", "stats": ".stats", "idea": ".idea", "card": ".demo-card", "header": ".site-header"} {
		p.ShotElement(t, sel, filepath.Join(dir, "detail-"+name+".png"), 1.5)
	}
	p.Navigate(t, base+"/demos/guestbook")
	p.ShotElement(t, ".demo-head", filepath.Join(dir, "detail-demo-head.png"), 1.5)
	p.ShotElement(t, ".demo-body", filepath.Join(dir, "detail-demo-body.png"), 1)
	p.Eval(t, "document.querySelector('.prefs-button').click()", nil)
	time.Sleep(300 * time.Millisecond)
	p.ShotElement(t, "#prefs", filepath.Join(dir, "detail-menu.png"), 2)

	p.Navigate(t, base+"/")
	p.Eval(t, "document.fonts.ready.then(() => true)", nil)
	p.ShotElement(t, ".quickstart", filepath.Join(dir, "detail-quickstart.png"), 1.5)

	m := b.NewPage(t, 390, 844)
	m.Navigate(t, base+"/docs/concepts/ticks")
	m.Wait(t, "customElements.get('sb-drawer')", 5*time.Second)
	m.Eval(t, "document.querySelector('.docs-menu-button').click()", nil)
	time.Sleep(700 * time.Millisecond)
	m.Screenshot(t, filepath.Join(dir, "mobile-docs-menu.png"), false)
	m.Navigate(t, base+"/")
	m.Eval(t, "document.cookie = 'lith-theme=light; Path=/'", nil)
	m.Navigate(t, base+"/demos")
	m.Wait(t, "customElements.get('sb-drawer')", 5*time.Second)
	m.Eval(t, "document.querySelector('.menu-button').click()", nil)
	time.Sleep(700 * time.Millisecond)
	m.Screenshot(t, filepath.Join(dir, "mobile-menu-light.png"), false)
	m.Navigate(t, base+"/demos/guestbook")
	time.Sleep(500 * time.Millisecond)
	m.Screenshot(t, filepath.Join(dir, "mobile-guestbook.png"), false)
}
