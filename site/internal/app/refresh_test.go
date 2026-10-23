package app_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
)

// TestRefreshAfterDeploy reconnects the guestbook's page as a page that a
// former build rendered does after a deploy. The parts around the demo are
// rendered afresh, while the demo keeps what was typed into it and the
// page keeps one stream; a page that loaded other scripts or stylesheets
// gets the notice instead.
func TestRefreshAfterDeploy(t *testing.T) {
	a, base := apptest.Start(t)
	p := browser.Start(t).NewPage(t, 1280, 900)
	p.Navigate(t, base+"/demos/guestbook")
	p.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)

	stale := func(signals string) {
		t.Helper()
		p.Eval(t, `(() => {
			const s = document.createElement('div')
			s.setAttribute('data-signals', `+strconv.Quote(signals)+`)
			document.body.append(s)
			document.body.insertAdjacentHTML('beforeend', '<span id="build-probe" data-text="$_build"></span>')
			document.querySelector('#demo-head h1').textContent = 'Stale'
			document.getElementById('demo-server').insertAdjacentHTML('beforeend', '<p id="stale">from the old build</p>')
			const input = document.querySelector('[data-bind="_guestbook.message"]')
			input.value = 'Typed'
			input.dispatchEvent(new Event('input', {bubbles: true}))
			return true
		})()`, nil)
		p.Wait(t, `document.getElementById('build-probe').textContent === 'old'`, 5*time.Second)
		p.Eval(t, `(() => { const el = document.getElementById('stream'); el.replaceWith(el.cloneNode(true)); return true })()`, nil)
	}

	stale(`{_build: 'old'}`)
	p.Wait(t, `document.querySelector('#demo-head h1').textContent === 'Guestbook' && !document.getElementById('stale') && document.getElementById('build-probe').textContent === 'test'`, 10*time.Second)
	var ok bool
	p.Eval(t, `document.querySelector('[data-bind="_guestbook.message"]').value === 'Typed' && document.getElementById('update').textContent.trim() === ''`, &ok)
	if !ok {
		t.Fatal("the refresh took the demo's input, or showed the notice")
	}
	if st := a.Hub().Stats(); st.Refreshes != 1 || st.Streams != 1 {
		t.Fatalf("%d refreshes, %d streams", st.Refreshes, st.Streams)
	}

	p.Eval(t, `(document.querySelectorAll('#build-probe').forEach(e => e.remove()), true)`, nil)
	stale(`{_build: 'old', _assets: 'old'}`)
	p.Wait(t, `document.getElementById('update').textContent.includes('This page has a new version.')`, 10*time.Second)
	p.Eval(t, `!!document.getElementById('stale') && document.querySelector('#demo-head h1').textContent === 'Stale'`, &ok)
	if !ok {
		t.Fatal("a page with other assets was morphed")
	}
	if errs := p.Errors(); len(errs) > 0 {
		t.Errorf("page errors: %v", errs)
	}
}
