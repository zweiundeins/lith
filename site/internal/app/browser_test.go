package app_test

import (
	"slices"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
)

// TestBrowser loads the landing page, a docs page and the guestbook in
// headless Chromium, with the site's CSP, and fails on any console error,
// uncaught exception or failed load. Two people open the guestbook; one
// signs; the other's page, and the landing page's slot, show the entry.
//
//	LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestBrowser
func TestBrowser(t *testing.T) {
	a, base := apptest.Start(t)
	b := browser.Start(t)
	clean := func(name string, p *browser.Page) {
		t.Helper()
		if errs := p.Errors(); len(errs) > 0 {
			t.Errorf("%s: %d errors:\n%v", name, len(errs), errs)
		}
	}

	home := b.NewPage(t, 1280, 900)
	home.Navigate(t, base+"/")
	home.Wait(t, `document.querySelector('[data-demo="guestbook"] .gb-embed__head.is-live')`, 10*time.Second)
	clean("the landing page", home)

	docs := b.NewPage(t, 1280, 900)
	docs.Navigate(t, base+"/docs/concepts/ticks")
	var current string
	docs.Eval(t, `document.querySelector('.docs-nav [aria-current="page"]').textContent`, &current)
	if current != "Ticks" {
		t.Errorf("the navigation marks %q", current)
	}
	docs.Eval(t, `document.querySelector('.pager__next').click()`, nil)
	docs.WaitLoad(t)
	docs.Wait(t, `document.querySelector('h1.page-title')?.textContent === 'Hypermedia'`, 5*time.Second)
	clean("the docs", docs)

	ada := b.NewPage(t, 1280, 900)
	ada.Navigate(t, base+"/demos/guestbook")
	bob := b.NewPage(t, 1280, 900)
	bob.Navigate(t, base+"/demos/guestbook")
	for _, p := range []*browser.Page{ada, bob} {
		p.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)
	}
	waitFor(t, "two tabs on the guestbook's list", func() bool { return a.Hub().Count("guestbook:entries") == 2 })
	// Bob's page records the view transitions it runs: the stream's must
	// be scoped to the element they patch, so that the rest of the page
	// keeps taking clicks while one runs.
	bob.Eval(t, `(() => {
		window.vts = []
		const el = HTMLElement.prototype.startViewTransition
		HTMLElement.prototype.startViewTransition = function (...args) { vts.push('#' + this.id); return el.apply(this, args) }
		const doc = document.startViewTransition
		document.startViewTransition = function (...args) { vts.push('document'); return doc.apply(this, args) }
	})()`, nil)

	ada.Eval(t, `(() => {
		const set = (sel, v) => { const el = document.querySelector(sel); el.value = v; el.dispatchEvent(new Event('input', {bubbles: true})) }
		set('[data-bind="_guestbook.name"]', 'Ada')
		set('[data-bind="_guestbook.message"]', 'Hello from a headless browser')
		document.querySelector('.gb__form button[type=submit]').click()
	})()`, nil)
	ada.Wait(t, `document.querySelector('#guestbook-outcome').textContent.includes('Thank you for signing.')`, 10*time.Second)
	ada.Wait(t, `document.querySelector('.gb__form button[type=submit]').getAttribute('aria-busy') !== 'true' && document.querySelector('[data-bind="_guestbook.message"]').value === ''`, 5*time.Second)
	bob.Wait(t, `document.querySelector('#guestbook-entries').textContent.includes('Hello from a headless browser')`, 10*time.Second)
	bob.Wait(t, `document.querySelector('#guestbook-entries li[data-fresh]')`, time.Second)
	var vts []string
	bob.Eval(t, `vts`, &vts)
	if !slices.Contains(vts, "#guestbook-entries") || slices.Contains(vts, "document") {
		t.Errorf("the entry arrived in the view transitions %q, want one scoped to #guestbook-entries", vts)
	}
	var outcome string
	bob.Eval(t, `document.querySelector('#guestbook-outcome').textContent.trim()`, &outcome)
	if outcome != "" {
		t.Errorf("the other tab hears %q about a signature it did not send", outcome)
	}
	home.Wait(t, `document.querySelector('#guestbook-latest').textContent.includes('Hello from a headless browser')`, 10*time.Second)

	// The display menu: the light theme, kept in a cookie.
	home.Eval(t, `document.querySelector('.prefs-button').click()`, nil)
	home.Wait(t, `document.querySelector('#prefs').matches(':popover-open')`, 2*time.Second)
	home.Eval(t, `document.querySelector('[data-pref="theme"][data-value="light"]').click()`, nil)
	home.Wait(t, `document.documentElement.dataset.lithTheme === 'light' && document.cookie.includes('lith-theme=light')`, 3*time.Second)

	for name, p := range map[string]*browser.Page{"the landing page": home, "the docs": docs, "Ada's guestbook": ada, "Bob's guestbook": bob} {
		clean(name, p)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 10 s for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
