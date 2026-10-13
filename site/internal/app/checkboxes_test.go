package app_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
)

// TestCheckboxes reads the million checkboxes from a tab's stream: the
// tab's list, which asks for the window, and the numbers at once; the
// chunks of the window once the page has sent it; a flip in the next tick;
// after a scroll the chunks of the new window and none of the old one;
// and after the stream comes back, the window the tab stored.
func TestCheckboxes(t *testing.T) {
	a, base := apptest.Start(t)
	c := client(t)
	get(t, c, base+"/demos/checkboxes") // the session's cookie

	events := listen(t, c, base+"/demos/checkboxes/stream?tab=tab1")
	cbSeen(t, events, `id="checkboxes-board"`, `{"_checkboxes":{"hello":true}}`, `id="checkboxes-stats"`)
	if n := a.Hub().Count("checkboxes:chunk:0"); n != 0 {
		t.Errorf("a stream watches %d chunks before the page sends its window", n)
	}
	// The page answers the list's first frame with its window (greet).
	if code := post(t, c, base+"/cmd/checkboxes/window", `{"tab":"tab1","offset":0,"count":12,"cols":2}`); code != http.StatusNoContent {
		t.Fatalf("the first window: %d", code)
	}
	first := next(t, events, `id="checkboxes-tile-0"`)
	if !strings.Contains(first, "selector #checkboxes-slot-0") || !strings.Contains(first, "mode inner") {
		t.Errorf("chunk 0's frame:\n%.300s", first)
	}
	waitFor(t, "the stream on the top's chunks", func() bool { return a.Hub().Count("checkboxes:chunk:11") == 1 })
	if n := a.Hub().Count("checkboxes:chunk:12"); n != 0 {
		t.Errorf("chunk 12, outside the window, has %d watchers", n)
	}

	if code := post(t, c, base+"/cmd/checkboxes/set", `{"box":5,"checked":true}`); code != http.StatusNoContent {
		t.Fatalf("set: %d", code)
	}
	tile := next(t, events, "title=6 checked")
	if strings.Count(tile, "<input") != 256 || strings.Count(tile, " checked>") != 1 || strings.Contains(tile, "checkboxes-stats") {
		t.Errorf("a flip's frame carries more than its tile:\n%.400s", tile)
	}
	next(t, events, `<dd>1</dd>`)

	// A scroll far down: the list at its new offset, the new window's
	// chunks, and the old window's no more.
	if code := post(t, c, base+"/cmd/checkboxes/window", `{"tab":"tab1","offset":2000,"count":6,"cols":2}`); code != http.StatusNoContent {
		t.Fatalf("window: %d", code)
	}
	cbSeen(t, events, `id="checkboxes-tile-2000"`, `offset="1998"`)
	waitFor(t, "the stream on the new window", func() bool {
		return a.Hub().Count("checkboxes:chunk:0") == 0 && a.Hub().Count("checkboxes:chunk:2009") == 1
	})
	post(t, c, base+"/cmd/checkboxes/set", `{"box":6,"checked":true}`)
	post(t, c, base+"/cmd/checkboxes/set", `{"box":512000,"checked":true}`)
	for _, e := range cbSeen(t, events, "title=512,001 checked") {
		if strings.Contains(e, `id="checkboxes-tile-0"`) {
			t.Error("a chunk the tab scrolled away from still reaches it")
		}
	}

	// The stream comes back, as after a deploy: the list's first frame,
	// at the window the tab stored, asks again, and the page's answer
	// brings that window's chunks.
	again := listen(t, c, base+"/demos/checkboxes/stream?tab=tab1")
	cbSeen(t, again, `offset="1998"`, `{"_checkboxes":{"hello":true}}`)
	post(t, c, base+"/cmd/checkboxes/window", `{"tab":"tab1","offset":1998,"count":12,"cols":2}`)
	next(t, again, `id="checkboxes-tile-2005"`)
	waitFor(t, "the window watched again", func() bool {
		return a.Hub().Count("checkboxes:chunk:2005") == 1 && a.Hub().Count("checkboxes:chunk:0") == 0
	})

	// What a page cannot send.
	for body, want := range map[string]int{
		`{"box":1000000,"checked":true}`: http.StatusBadRequest,
		`{"box":-1,"checked":true}`:      http.StatusBadRequest,
		`not json`:                       http.StatusBadRequest,
	} {
		if code := post(t, c, base+"/cmd/checkboxes/set", body); code != want {
			t.Errorf("set %s: %d", body, code)
		}
	}
	for _, body := range []string{`{"tab":"bad tab!","offset":0,"count":4,"cols":1}`, `{"tab":"tab1","offset":3907,"count":4,"cols":1}`} {
		if code := post(t, c, base+"/cmd/checkboxes/window", body); code != http.StatusBadRequest {
			t.Errorf("window %s: %d", body, code)
		}
	}

	// A burst of clicks runs into the session's rate.
	codes := map[int]int{}
	for i := range 60 {
		codes[post(t, c, base+"/cmd/checkboxes/set", fmt.Sprintf(`{"box":%d,"checked":true}`, 1000+i))]++
	}
	if codes[http.StatusTooManyRequests] == 0 {
		t.Errorf("60 clicks at once: %v", codes)
	}

	// The sweep keeps an hour of seconds.
	if err := a.SweepNow(context.Background(), time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var seconds int
	a.DB().R.QueryRow(`SELECT count(*) FROM checkboxes_seconds`).Scan(&seconds)
	if seconds != 0 {
		t.Errorf("the sweep left %d seconds", seconds)
	}
}

// TestCheckboxesBrowser has two people on the grid in Chromium. Ada ticks
// a box while the writer is held: her box waits, indeterminate, and does
// not tick until the frame comes; Bob's page shows the box once it does.
// Then both jump to box 500,000, their streams leave the top's chunks, and
// Bob ticks the box for Ada to see.
//
//	LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestCheckboxesBrowser
func TestCheckboxesBrowser(t *testing.T) {
	a, base := apptest.Start(t)
	b := browser.Start(t)
	ada := b.NewPage(t, 1280, 900)
	ada.Navigate(t, base+"/demos/checkboxes")
	bob := b.NewPage(t, 390, 844)
	bob.Navigate(t, base+"/demos/checkboxes")
	for _, p := range []*browser.Page{ada, bob} {
		p.Wait(t, `document.querySelector('.live-badge.is-live') && customElements.get('sb-virtual-scroll')`, 10*time.Second)
	}
	waitFor(t, "two tabs on chunk 0", func() bool { return a.Hub().Count("checkboxes:chunk:0") == 2 })
	bob.Wait(t, `document.querySelector('#checkboxes-stats .cb-stat:last-child dd')?.textContent === '2'`, 5*time.Second)

	const box1 = `document.querySelector('#checkboxes-tile-0 .cb-boxes').children[0]`
	held := make(chan struct{})
	release := make(chan struct{})
	go a.Writer().Exclusive(context.Background(), func(context.Context, *sql.Conn) error {
		close(held)
		<-release
		return nil
	})
	<-held
	var now struct{ Checked, Pending bool }
	ada.Eval(t, box1+`.click()`, nil)
	ada.Wait(t, box1+`.indeterminate`, 3*time.Second)
	ada.Eval(t, `({checked: `+box1+`.checked, pending: `+box1+`.indeterminate})`, &now)
	if now.Checked || !now.Pending {
		t.Errorf("before the frame Ada's box shows %+v", now)
	}
	close(release)
	ada.Wait(t, box1+`.checked && !`+box1+`.indeterminate`, 5*time.Second)
	bob.Wait(t, box1+`.checked`, 5*time.Second)
	bob.Wait(t, `document.querySelector('#checkboxes-stats dd').textContent === '1'`, 5*time.Second)

	jump := func(p *browser.Page, n int) {
		t.Helper()
		p.Eval(t, fmt.Sprintf(`(() => {
			const input = document.querySelector('.cb-jump input')
			input.value = '%d'
			input.dispatchEvent(new Event('input', {bubbles: true}))
			document.querySelector('.cb-jump button').click()
		})()`, n), nil)
	}
	const far = `document.querySelector('#checkboxes-tile-1953 .cb-boxes').children[31]`
	jump(bob, 500_000)
	bob.Wait(t, `document.activeElement === `+far, 5*time.Second)
	waitFor(t, "Bob's stream on the new window", func() bool {
		return a.Hub().Count("checkboxes:chunk:0") == 1 && a.Hub().Count("checkboxes:chunk:1953") == 1
	})
	jump(ada, 500_000)
	ada.Wait(t, `document.activeElement === `+far, 5*time.Second)
	waitFor(t, "both on the new window", func() bool { return a.Hub().Count("checkboxes:chunk:1953") == 2 })

	// Bob ticks it with the keyboard; Ada sees it.
	cbPressSpace(t, bob)
	bob.Wait(t, far+`.checked`, 5*time.Second)
	ada.Wait(t, far+`.checked`, 5*time.Second)

	// The arrows walk the boxes: right from the end of a row to the next
	// row's start, down a row of 16, and on into the next tile.
	for _, step := range []struct{ key, title string }{{"ArrowRight", "500,001"}, {"ArrowDown", "500,017"}, {"ArrowLeft", "500,016"}, {"ArrowUp", "500,000"}} {
		cbPressKey(t, bob, step.key)
		bob.Wait(t, `document.activeElement.title === '`+step.title+`'`, 2*time.Second)
	}
	bob.Eval(t, `document.querySelector('#checkboxes-tile-1953 .cb-boxes').children[255].focus()`, nil)
	cbPressKey(t, bob, "ArrowDown")
	bob.Wait(t, `document.activeElement.title === '500,240'`, 2*time.Second)
	var label string
	ada.Eval(t, `document.querySelector('#checkboxes-tile-1953').getAttribute('aria-label')`, &label)
	if label != "Boxes 499,969 to 500,224, 1 checked" {
		t.Errorf("the tile's label: %q", label)
	}

	for name, p := range map[string]*browser.Page{"Ada": ada, "Bob": bob} {
		if errs := p.Errors(); len(errs) > 0 {
			t.Errorf("%s: %v", name, errs)
		}
		// Not even a warning: every frame found its slot.
		for _, l := range p.Logs() {
			if strings.HasPrefix(l, "console.warn") {
				t.Errorf("%s: %s", name, l)
			}
		}
	}
}

// cbSeen reads events until each of wants has been in one, in any order, and
// returns the events it read.
func cbSeen(t *testing.T, events <-chan string, wants ...string) []string {
	t.Helper()
	var read []string
	missing := map[string]bool{}
	for _, w := range wants {
		missing[w] = true
	}
	timeout := time.After(5 * time.Second)
	for len(missing) > 0 {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("the stream ended before %v", missing)
			}
			read = append(read, event)
			for w := range missing {
				if strings.Contains(event, w) {
					delete(missing, w)
				}
			}
		case <-timeout:
			t.Fatalf("no event with %v in 5s", missing)
		}
	}
	return read
}

// cbPressSpace presses the space bar on the focused element.
func cbPressSpace(t *testing.T, p *browser.Page) {
	t.Helper()
	for _, typ := range []string{"keyDown", "keyUp"} {
		p.Call(t, "Input.dispatchKeyEvent", map[string]any{"type": typ, "key": " ", "code": "Space", "windowsVirtualKeyCode": 32, "text": " "})
	}
}

// cbPressKey presses a key that types nothing, such as an arrow.
func cbPressKey(t *testing.T, p *browser.Page, key string) {
	t.Helper()
	codes := map[string]int{"ArrowLeft": 37, "ArrowUp": 38, "ArrowRight": 39, "ArrowDown": 40}
	for _, typ := range []string{"rawKeyDown", "keyUp"} {
		p.Call(t, "Input.dispatchKeyEvent", map[string]any{"type": typ, "key": key, "code": key, "windowsVirtualKeyCode": codes[key]})
	}
}
