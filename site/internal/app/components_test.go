package app_test

import (
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
)

// TestStarbaseComponents loads the vendored Starbase components the way
// the pages do, through the import map onto the site's Datastar build: on
// a phone-sized docs page the menu (sb-drawer) opens with the docs'
// navigation, Escape closes it and gives the focus back; on the landing
// page the quick start's sb-tabs switches panels.
func TestStarbaseComponents(t *testing.T) {
	_, base := apptest.Start(t)
	b := browser.Start(t)

	phone := b.NewPage(t, 390, 844)
	phone.Navigate(t, base+"/docs/concepts/ticks")
	phone.Wait(t, `customElements.get('sb-drawer') && document.querySelector('.menu-button').checkVisibility()`, 5*time.Second)
	var hidden bool
	phone.Eval(t, `!document.querySelector('[data-docs-nav]').checkVisibility()`, &hidden)
	if !hidden {
		t.Error("the docs' sidebar shows on a phone")
	}
	phone.Eval(t, `document.querySelector('.docs-menu-button').focus(); document.querySelector('.docs-menu-button').click()`, nil)
	phone.Wait(t, `document.querySelector('sb-drawer').isOpen`, 3*time.Second)
	var links int
	phone.Eval(t, `document.querySelectorAll('sb-drawer .docs-nav a').length`, &links)
	if links < 10 {
		t.Errorf("the menu holds %d docs links", links)
	}
	phone.Press(t, "Escape")
	phone.Wait(t, `!document.querySelector('sb-drawer').isOpen`, 3*time.Second)
	phone.Wait(t, `document.activeElement === document.querySelector('.docs-menu-button')`, 2*time.Second)

	home := b.NewPage(t, 1280, 900)
	home.Navigate(t, base+"/")
	home.Wait(t, `customElements.get('sb-tabs') && document.querySelector('.starts').shadowRoot`, 5*time.Second)
	home.Eval(t, `document.querySelector('.starts').selected = 2`, nil)
	home.Wait(t, `document.querySelector('[slot="from-a-checkout"]').checkVisibility() && !document.querySelector('[slot="new-project"]').checkVisibility()`, 3*time.Second)

	for name, p := range map[string]*browser.Page{"the phone": phone, "the landing page": home} {
		if errs := p.Errors(); len(errs) > 0 {
			t.Errorf("%s: %v", name, errs)
		}
	}
}
