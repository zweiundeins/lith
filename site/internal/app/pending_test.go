package app_test

import (
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
)

// TestPendingAcrossAReconnect signs the guestbook twice. The first
// signature's request fails with a 500 whose notice is lost (the page's
// fetch answers it, so no notice is sent): the 500 ends nothing, and when
// the tab hides and shows again, its stream opens again, the guestbook's
// tab view does not ack the op, and the page ends the pending state in its
// own words. The second goes out while the tab is hidden, so its stream is
// closed: the site takes no action for a tab without an open stream and
// answers a bare 400, which comes without a notice, so the page ends the
// op at once. Both keep the message.
func TestPendingAcrossAReconnect(t *testing.T) {
	a, base := apptest.Start(t)
	p := browser.Start(t).NewPage(t, 1280, 900)
	p.Navigate(t, base+"/demos/guestbook")
	p.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)
	p.Eval(t, `(document.addEventListener('datastar-fetch', e => { if (e.detail.type === 'error') window.lastStatus = e.detail.argsRaw.status }), true)`, nil)

	visibility := func(hidden bool) {
		t.Helper()
		h := "false"
		if hidden {
			h = "true"
		}
		p.Eval(t, `(() => {
			Object.defineProperty(document, 'hidden', {get: () => `+h+`, configurable: true})
			Object.defineProperty(document, 'visibilityState', {get: () => `+h+` ? 'hidden' : 'visible', configurable: true})
			document.dispatchEvent(new Event('visibilitychange', {bubbles: true}))
			return true
		})()`, nil)
	}
	sign := func(message string) {
		t.Helper()
		p.Eval(t, `(() => {
			window.lastStatus = undefined
			const set = (sel, v) => { const el = document.querySelector(sel); el.value = v; el.dispatchEvent(new Event('input', {bubbles: true})) }
			set('[data-bind="_guestbook.name"]', 'Ada')
			set('[data-bind="_guestbook.message"]', '`+message+`')
			document.querySelector('.gb__form button[type=submit]').click()
			return true
		})()`, nil)
	}
	const pending = `document.querySelector('.gb__form button[type=submit]').getAttribute('aria-busy') === 'true'`
	ended := func(problem, message string) string {
		return `!(` + pending + `) && document.querySelector('.gb .is-problem').textContent === '` + problem + `' && document.querySelector('[data-bind="_guestbook.message"]').value === '` + message + `'`
	}

	p.Eval(t, `(() => {
		const fetch = window.fetch
		window.fetch = (url, init) => String(url).includes('/cmd/guestbook/sign') && !window.passSign ? Promise.resolve(new Response(null, {status: 500})) : fetch(url, init)
		return true
	})()`, nil)
	sign("Lost to a reconnect")
	p.Wait(t, `window.lastStatus === '500'`, 5*time.Second)
	time.Sleep(300 * time.Millisecond)
	var busy bool
	p.Eval(t, pending, &busy)
	if !busy {
		t.Fatal("the 500 ended the pending state")
	}
	visibility(true) // Datastar closes the stream
	waitFor(t, "the stream to close", func() bool { return a.Hub().Count("guestbook:entries") == 0 })
	visibility(false) // Datastar opens the stream again
	p.Wait(t, ended("That did not go through. Try again.", "Lost to a reconnect"), 10*time.Second)

	p.Eval(t, `(window.passSign = true)`, nil)
	visibility(true)
	waitFor(t, "the stream to close", func() bool { return a.Hub().Count("guestbook:entries") == 0 })
	sign("No stream")
	p.Wait(t, `window.lastStatus === '400'`, 5*time.Second)
	p.Wait(t, ended("That did not work. Try again.", "No stream"), 5*time.Second)
	for _, e := range p.Errors() {
		if !strings.HasPrefix(e, "error network:") {
			t.Errorf("page error: %s", e)
		}
	}
}
