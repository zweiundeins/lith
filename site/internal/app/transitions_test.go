package app_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
)

// TestTransitionTypes follows links in Chromium and checks the type each
// cross-document view transition gets (transitions.js): into a demo from
// the landing page zooms in, along the docs slides forward and back, home
// again zooms out. transitions.js leaves the type it chose on <html>.
func TestTransitionTypes(t *testing.T) {
	_, base := apptest.Start(t)
	b := browser.Start(t)
	p := b.NewPage(t, 1280, 900)
	p.Navigate(t, base+"/")
	follow := func(click, want string) {
		t.Helper()
		p.Eval(t, click, nil)
		p.WaitLoad(t)
		// pagereveal runs at the new page's first frame, which can come
		// after its load event.
		p.Wait(t, `document.documentElement.dataset.lithTransition`, 5*time.Second)
		var got string
		p.Eval(t, `document.documentElement.dataset.lithTransition`, &got)
		if got != want {
			t.Errorf("%s: a transition of type %q, want %s", click, got, want)
		}
		p.Wait(t, `!document.documentElement.matches(':active-view-transition')`, 5*time.Second)
	}
	follow(`document.querySelector('[data-demo-card="guestbook"] a').click()`, "zoom-in")
	follow(`location.href = '/docs/concepts/ticks'`, "fade")
	follow(`document.querySelector('.pager__next').click()`, "forward")
	follow(`document.querySelector('.pager__prev').click()`, "backward")
	follow(`document.querySelector('.brand').click()`, "zoom-out")
	if errs := p.Errors(); len(errs) > 0 {
		t.Errorf("errors: %v", errs)
	}
}

// TestTransitionShots writes a sequence of screenshots of transitions, to
// look at: a demo's card morphing into its page's head, a docs page
// sliding forward, and the theme switch's circle.
//
//	LITH_SITE_SHOTS=/tmp/shots LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestTransitionShots
func TestTransitionShots(t *testing.T) {
	dir := os.Getenv("LITH_SITE_SHOTS")
	if dir == "" {
		t.Skip("set LITH_SITE_SHOTS to a directory for the screenshots")
	}
	_, base := apptest.Start(t)
	b := browser.Start(t)
	p := b.NewPage(t, 1280, 800)
	p.Call(t, "Animation.enable", nil)
	sequence := func(name, start, action string, frames int, every time.Duration) {
		p.Navigate(t, base+start)
		p.Eval(t, "document.fonts.ready.then(() => true)", nil)
		time.Sleep(800 * time.Millisecond)
		if name == "card" {
			p.Eval(t, `document.querySelector('[data-demo-card="guestbook"]').scrollIntoView({block: 'center'})`, nil)
			time.Sleep(300 * time.Millisecond)
		}
		p.Call(t, "Animation.setPlaybackRate", map[string]any{"playbackRate": 0.1})
		p.Eval(t, action, nil)
		for i := range frames {
			time.Sleep(every)
			p.Call(t, "Animation.setPlaybackRate", map[string]any{"playbackRate": 0.1})
			p.Screenshot(t, filepath.Join(dir, fmt.Sprintf("vt-%s-%d.png", name, i)), false)
		}
		p.Call(t, "Animation.setPlaybackRate", map[string]any{"playbackRate": 1})
		time.Sleep(time.Second)
	}
	sequence("card", "/", `document.querySelector('[data-demo-card="guestbook"] a').click()`, 6, 700*time.Millisecond)
	sequence("docs", "/docs/concepts/ticks", `document.querySelector('.docs-nav a[href="/docs/concepts/hypermedia"]').click()`, 6, 700*time.Millisecond)
	sequence("theme", "/docs/concepts/ticks", `document.querySelector('.prefs-button').click(); document.querySelector('[data-pref="theme"][data-value="light"]').click()`, 6, 700*time.Millisecond)
}
