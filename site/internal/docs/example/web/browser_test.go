package web_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zweiundeins/lith/edge"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/browser"
	"github.com/zweiundeins/lith/site/internal/docs/example/board"
)

const (
	pendingJS = `document.querySelector('form input').hasAttribute('readonly')`
	problemJS = `document.querySelector('.problem').textContent`
	lostText  = `'That did not go through. Try again.'`
	lostJS    = `!` + pendingJS + ` && ` + problemJS + ` === 'That did not go through. Try again.' && document.querySelector('form input').value === 'Lost card'`
)

// open loads board 1 in a new page and waits for its stream. The page
// records the status of an action's failed answer in window.lastStatus.
func open(t *testing.T, s server) *browser.Page {
	t.Helper()
	p := browser.Start(t).NewPage(t, 1000, 800)
	p.Navigate(t, s.base+"/boards/1")
	p.Wait(t, `document.querySelector('#presence').textContent === '1 tab on this board'`, 10*time.Second)
	p.Eval(t, `(document.addEventListener('datastar-fetch', e => { if (e.detail.type === 'error') window.lastStatus = e.detail.argsRaw.status }), true)`, nil)
	return p
}

// scriptErrors are the page's errors but those of the network: the
// favicon the example has none of, and the statuses a test causes.
func scriptErrors(p *browser.Page) []string {
	var out []string
	for _, e := range p.Errors() {
		if !strings.HasPrefix(e, "error network:") {
			out = append(out, e)
		}
	}
	return out
}

// add types a title into the add form and submits it.
func add(t *testing.T, p *browser.Page, title string) {
	t.Helper()
	p.Eval(t, `(() => {
		const input = document.querySelector('form input')
		input.value = `+"`"+title+"`"+`
		input.dispatchEvent(new Event('input', {bubbles: true}))
		document.querySelector('form button[type=submit]').click()
		return true
	})()`, nil)
}

// visible makes the page hidden or shown as the browser reports it, and
// with tell dispatches visibilitychange, as a browser does when a tab
// hides or shows.
func visible(t *testing.T, p *browser.Page, shown, tell bool) {
	t.Helper()
	hidden := "true"
	if shown {
		hidden = "false"
	}
	dispatch := ""
	if tell {
		dispatch = `document.dispatchEvent(new Event('visibilitychange', {bubbles: true}))`
	}
	p.Eval(t, `(() => {
		Object.defineProperty(document, 'hidden', {get: () => `+hidden+`, configurable: true})
		Object.defineProperty(document, 'visibilityState', {get: () => `+hidden+` ? 'hidden' : 'visible', configurable: true})
		`+dispatch+`
		return true
	})()`, nil)
}

// reconnect has the page open its stream again, as sse.Reconnect does.
func reconnect(t *testing.T, p *browser.Page) {
	t.Helper()
	p.Eval(t, `(() => { const el = document.getElementById('stream'); el.replaceWith(el.cloneNode(true)); return true })()`, nil)
}

// closed waits until the hub has no stream open.
func closed(t *testing.T, s server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.hub.Stats().Streams > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the tab's stream is still open")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// refused sends an action from the page that the rate limit turns away
// while the tab has no stream, so its notice goes nowhere, and checks
// that the 429 ends nothing.
func refused(t *testing.T, s server, p *browser.Page) {
	t.Helper()
	post(t, client(t), s.base+"/cmd/move", `{"tab":"drain","key":"card-7","op":"drain1","card":7,"lane":2,"seen":1}`) // the one action the limit allows
	add(t, p, "Lost card")
	p.Wait(t, `window.lastStatus === '429'`, 5*time.Second)
	time.Sleep(300 * time.Millisecond)
	var pending bool
	p.Eval(t, pendingJS, &pending)
	if !pending {
		t.Fatal("the 429 ended the pending state")
	}
}

// blockWriter holds the writer in a command until the func it returns is
// called, or the test ends.
func blockWriter(t *testing.T, s server) func() {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free)
	if err := s.w.Send(writer.Func(func(tx *writer.Tx) error {
		<-release
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	return free
}

// still fails unless the page's op is pending without a problem shown.
func still(t *testing.T, p *browser.Page) {
	t.Helper()
	var pending bool
	var problem string
	p.Eval(t, pendingJS, &pending)
	p.Eval(t, problemJS, &problem)
	if !pending || problem != "" {
		t.Fatalf("pending %v, problem %q", pending, problem)
	}
}

// TestPendingUntilTheAck adds a card while the writer is busy with a long
// command: nothing on the page ends the pending state, however long the
// command takes, until the stream brings the op's ack.
func TestPendingUntilTheAck(t *testing.T) {
	s := startWith(t, nil)
	p := open(t, s)
	free := blockWriter(t, s)
	add(t, p, "Slow card")
	p.Wait(t, pendingJS, 5*time.Second)
	time.Sleep(3 * time.Second)
	still(t, p)
	free()
	p.Wait(t, `!`+pendingJS+` && document.querySelector('form input').value === '' && document.querySelector('#board').textContent.includes('Slow card')`, 5*time.Second)
	if errs := scriptErrors(p); len(errs) > 0 {
		t.Errorf("page errors: %v", errs)
	}
}

// staleBuild makes the page one that a former build rendered: its $_build
// is "old", and #content holds an element no build renders. A probe
// outside #content shows $_build.
func staleBuild(t *testing.T, p *browser.Page) {
	t.Helper()
	p.Eval(t, `(() => {
		const main = document.querySelector('main')
		main.insertAdjacentHTML('beforeend', '<div data-signals="{_build: \'old\'}"></div><span id="build-probe" data-text="$_build"></span>')
		document.getElementById('content').insertAdjacentHTML('afterbegin', '<p id="stale">from the old build</p>')
		return true
	})()`, nil)
	p.Wait(t, `document.getElementById('build-probe').textContent === 'old'`, 5*time.Second)
}

// TestRefreshAfterDeploy reconnects pages as pages that a former build
// rendered do after a deploy. A page without typed input gets #content
// rendered afresh, keeps one stream, and its $_build becomes the running
// build; a page with a title the person typed gets the notice instead,
// and keeps the title and its old content.
func TestRefreshAfterDeploy(t *testing.T) {
	s := startWith(t, nil)
	p := open(t, s)
	staleBuild(t, p)
	reconnect(t, p)
	p.Wait(t, `!document.getElementById('stale') && document.getElementById('build-probe').textContent === 'now' && document.querySelector('#presence').textContent === '1 tab on this board'`, 10*time.Second)
	var ok bool
	p.Eval(t, `document.querySelector('h1').textContent === 'Launch' && document.querySelectorAll('#board .lane').length > 0 && document.getElementById('update').textContent === ''`, &ok)
	if !ok {
		t.Fatal("the refreshed page lacks its board")
	}
	if st := s.hub.Stats(); st.Refreshes != 1 || st.Streams != 1 {
		t.Fatalf("%d refreshes, %d streams", st.Refreshes, st.Streams)
	}

	p.Eval(t, `(() => {
		const input = document.querySelector('form input')
		input.value = 'Draft'
		input.dispatchEvent(new Event('input', {bubbles: true}))
		return true
	})()`, nil)
	staleBuild(t, p)
	reconnect(t, p)
	p.Wait(t, `document.getElementById('update').textContent.includes('This board has a new version.') && document.getElementById('build-probe').textContent === 'now'`, 10*time.Second)
	p.Eval(t, `!!document.getElementById('stale') && document.querySelector('form input').value === 'Draft'`, &ok)
	if !ok {
		t.Fatal("the notice took the page's content or its title")
	}
	if errs := scriptErrors(p); len(errs) > 0 {
		t.Errorf("page errors: %v", errs)
	}
}

// synced waits until the page's stream is open again and its tab view has
// spoken, and fails unless the hub counts refreshes and one stream.
func synced(t *testing.T, s server, p *browser.Page, refreshes int64) {
	t.Helper()
	p.Wait(t, `document.getElementById('synced-probe').textContent === 'true'`, 20*time.Second)
	if st := s.hub.Stats(); st.Refreshes != refreshes || st.Streams != 1 {
		t.Fatalf("%d refreshes, %d streams; want %d and 1", st.Refreshes, st.Streams, refreshes)
	}
}

// TestRefreshAfterReopen reopens the stream of a refreshed page in the
// ways that run no data-init: the tab hidden and shown again, and a retry
// after the connection broke. Datastar keeps a @get's headers from its
// first request then, but reads the signals afresh, so the page reports
// the running build and is not refreshed again, with one stream open. A
// page of a former build where the person typed reports that too, and
// gets the notice, not the morph.
func TestRefreshAfterReopen(t *testing.T) {
	s := startWith(t, nil)
	p := open(t, s)
	p.Eval(t, `(document.querySelector('main').insertAdjacentHTML('beforeend', '<span id="synced-probe" data-text="$_synced"></span>'), true)`, nil)
	staleBuild(t, p)
	reconnect(t, p)
	p.Wait(t, `!document.getElementById('stale') && document.getElementById('build-probe').textContent === 'now'`, 10*time.Second)
	synced(t, s, p, 1)
	for range 2 {
		visible(t, p, false, true)
		closed(t, s)
		visible(t, p, true, true)
		synced(t, s, p, 1)
	}
	s.block.Store(true)
	s.srv.CloseClientConnections() // the stream breaks; its retries get 503
	closed(t, s)
	p.Wait(t, `document.getElementById('synced-probe').textContent === 'false'`, 5*time.Second)
	s.block.Store(false)
	synced(t, s, p, 1)

	p.Eval(t, `(() => {
		const input = document.querySelector('form input')
		input.value = 'Draft'
		input.dispatchEvent(new Event('input', {bubbles: true}))
		return true
	})()`, nil)
	staleBuild(t, p)
	visible(t, p, false, true)
	closed(t, s)
	visible(t, p, true, true)
	p.Wait(t, `document.getElementById('update').textContent.includes('This board has a new version.') && document.getElementById('build-probe').textContent === 'now'`, 10*time.Second)
	synced(t, s, p, 2)
	var ok bool
	p.Eval(t, `!!document.getElementById('stale') && document.querySelector('form input').value === 'Draft'`, &ok)
	if !ok {
		t.Fatal("the reopen morphed the page the person typed into")
	}
	if errs := scriptErrors(p); len(errs) > 0 {
		t.Errorf("page errors: %v", errs)
	}
}

// TestReconnectWhileInFlight reconnects the stream while the op's request
// still waits in the writer: its first frame lacks the ack, and nothing is
// decided yet, so the op stays pending until its ack, and one card comes
// of it.
func TestReconnectWhileInFlight(t *testing.T) {
	s := startWith(t, nil)
	p := open(t, s)
	free := blockWriter(t, s)
	add(t, p, "Slow card")
	p.Wait(t, pendingJS, 5*time.Second)
	reconnect(t, p)
	time.Sleep(time.Second)
	still(t, p)
	free()
	p.Wait(t, `!`+pendingJS+` && document.querySelector('form input').value === ''`, 10*time.Second)
	var cards int
	p.Eval(t, `[...document.querySelectorAll('#board li')].filter(li => li.textContent.includes('Slow card')).length`, &cards)
	var problem string
	p.Eval(t, problemJS, &problem)
	if cards != 1 || problem != "" {
		t.Fatalf("%d cards, problem %q", cards, problem)
	}
}

// TestPendingEndsOnReconnect sends an action while the tab's stream is
// closed: its 429's notice finds no stream. When the stream connects
// again, the tab's row does not ack the op, so the page ends the pending
// state in its own words and keeps the title: whether the page opens the
// stream again from its element (data-init), Datastar retries it after
// the connection broke (retrying), or the tab shows again after it was
// hidden (visibilitychange).
func TestPendingEndsOnReconnect(t *testing.T) {
	t.Run("data-init", func(t *testing.T) {
		s := startWith(t, edge.NewLimiter(1e-6, 1)) // one action, then none
		p := open(t, s)
		visible(t, p, false, true) // Datastar closes the stream
		closed(t, s)
		refused(t, s, p)
		visible(t, p, true, false)
		reconnect(t, p)
		p.Wait(t, lostJS, 10*time.Second)
	})
	t.Run("retrying", func(t *testing.T) {
		s := startWith(t, edge.NewLimiter(1e-6, 1))
		p := open(t, s)
		s.block.Store(true)
		s.srv.CloseClientConnections() // the stream breaks; its retries get 503
		closed(t, s)
		refused(t, s, p)
		s.block.Store(false)
		p.Wait(t, lostJS, 20*time.Second)
	})
	t.Run("visibilitychange", func(t *testing.T) {
		s := startWith(t, edge.NewLimiter(1e-6, 1))
		p := open(t, s)
		visible(t, p, false, true)
		closed(t, s)
		refused(t, s, p)
		visible(t, p, true, true) // Datastar opens the stream again
		p.Wait(t, lostJS, 10*time.Second)
		if errs := scriptErrors(p); len(errs) > 0 {
			t.Errorf("page errors: %v", errs)
		}
	})
}

// hold answers the requests to path with code alone, once the func it
// returns is called or the test ends: the board's handler never sees
// them, as if their notice went to a stream that is gone.
func (s server) hold(t *testing.T, path string, code int) func() {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free)
	s.hooks.Store(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(code)
	}))
	return free
}

// cards adds cards to the first lane and waits until the page shows them.
func cards(t *testing.T, s server, p *browser.Page, titles ...string) {
	t.Helper()
	for _, title := range titles {
		if err := s.w.Do(context.Background(), board.AddCard{Lane: 1, Title: title}); err != nil {
			t.Fatal(err)
		}
	}
	p.Wait(t, `document.querySelectorAll('#lane-1 li').length === `+strconv.Itoa(len(titles)), 5*time.Second)
}

// moveOn clicks the button that moves a card on.
func moveOn(t *testing.T, p *browser.Page, card int) {
	t.Helper()
	p.Eval(t, `(document.querySelector('#card-`+strconv.Itoa(card)+` button[data-lane]').click(), true)`, nil)
}

// faded is true while a card's move waits for its ack.
func faded(card int) string {
	return `getComputedStyle(document.getElementById('card-` + strconv.Itoa(card) + `')).opacity === '0.5'`
}

// TestLostWhileInFlight reconnects the stream while the op's request is
// out; the request then fails, and its notice went to the stream that is
// gone. When the request ends, the page opens the stream once more, and
// that frame, without the ack, ends the op in the page's own words.
func TestLostWhileInFlight(t *testing.T) {
	s := startWith(t, nil)
	p := open(t, s)
	answer := s.hold(t, "/cmd/add", http.StatusInternalServerError)
	add(t, p, "Lost card")
	p.Wait(t, pendingJS, 5*time.Second)
	reconnect(t, p)
	time.Sleep(time.Second)
	still(t, p)
	answer()
	p.Wait(t, lostJS, 10*time.Second)
}

// TestReopenBringsTheAck reconnects the stream while the op waits in the
// writer, and its commit then tells no hub, as one in another process
// does during a blue/green deploy. The stream opened again once the
// request ended brings the tab's row, and with it the ack.
func TestReopenBringsTheAck(t *testing.T) {
	s := startWith(t, nil)
	p := open(t, s)
	free := blockWriter(t, s)
	add(t, p, "Quiet card")
	p.Wait(t, pendingJS, 5*time.Second)
	reconnect(t, p)
	time.Sleep(time.Second)
	still(t, p)
	s.mute.Store(true)
	free()
	p.Wait(t, `!`+pendingJS+` && document.querySelector('form input').value === '' && document.querySelector('#board').textContent.includes('Quiet card')`, 10*time.Second)
	var problem string
	p.Eval(t, problemJS, &problem)
	if problem != "" {
		t.Fatalf("problem %q", problem)
	}
}

// TestReopenBringsTheAckBesideAnotherTab is TestReopenBringsTheAck with a
// second tab on the board. The reopen renders the tab's own row afresh,
// which ends the op; the board, which the other tab watches too, comes
// from the hub's kept frame until hub.MarkAll, which a new process calls
// once the old one has stopped.
func TestReopenBringsTheAckBesideAnotherTab(t *testing.T) {
	s := startWith(t, nil)
	p := open(t, s)
	q := browser.Start(t).NewPage(t, 1000, 800)
	q.Navigate(t, s.base+"/boards/1")
	q.Wait(t, `document.querySelector('#presence').textContent === '2 tabs on this board'`, 10*time.Second)
	free := blockWriter(t, s)
	add(t, p, "Quiet card")
	p.Wait(t, pendingJS, 5*time.Second)
	reconnect(t, p)
	time.Sleep(time.Second)
	still(t, p)
	s.mute.Store(true)
	free()
	p.Wait(t, `!`+pendingJS+` && document.querySelector('form input').value === ''`, 10*time.Second)
	s.hub.MarkAll()
	shown := `document.querySelector('#board').textContent.includes('Quiet card')`
	p.Wait(t, shown, 5*time.Second)
	q.Wait(t, shown, 5*time.Second)
}

// TestOpOutlivesItsForm removes the form while its op's request is out,
// as a closing modal or a morph does. The request still counts as ended,
// so the op it lost ends when the stream connects again.
func TestOpOutlivesItsForm(t *testing.T) {
	s := startWith(t, nil)
	p := open(t, s)
	answer := s.hold(t, "/cmd/add", http.StatusInternalServerError)
	add(t, p, "Lost card")
	p.Wait(t, pendingJS, 5*time.Second)
	p.Eval(t, `(document.querySelector('form').remove(), true)`, nil)
	answer()
	p.Wait(t, `document.getElementById('ops').children.length === 0`, 5*time.Second)
	reconnect(t, p)
	p.Wait(t, problemJS+` === `+lostText, 10*time.Second)
}

// TestHungRequestBlocksNoOther keeps a card's move unanswered and loses an
// add: each op key counts its own requests, so the reconnect ends the add
// while the move, still out, stays pending.
func TestHungRequestBlocksNoOther(t *testing.T) {
	s := startWith(t, nil)
	p := open(t, s)
	cards(t, s, p, "Stuck card")
	s.hold(t, "/cmd/move", http.StatusInternalServerError)
	moveOn(t, p, 1)
	p.Wait(t, faded(1), 5*time.Second)
	answer := s.hold(t, "/cmd/add", http.StatusInternalServerError)
	add(t, p, "Lost card")
	p.Wait(t, pendingJS, 5*time.Second)
	answer()
	reconnect(t, p)
	p.Wait(t, lostJS, 10*time.Second)
	var stuck bool
	p.Eval(t, faded(1), &stuck)
	if !stuck {
		t.Fatal("the move ended while its request was out")
	}
}

// TestTwoMovesToOneURL moves two cards while the writer is busy: both
// requests go to /cmd/move, and neither cancels the other.
func TestTwoMovesToOneURL(t *testing.T) {
	s := startWith(t, nil)
	p := open(t, s)
	cards(t, s, p, "First", "Second")
	free := blockWriter(t, s)
	moveOn(t, p, 1)
	moveOn(t, p, 2)
	p.Wait(t, faded(1)+` && `+faded(2), 5*time.Second)
	free()
	p.Wait(t, `document.querySelectorAll('#lane-2 li').length === 2 && document.getElementById('ops').children.length === 0`, 10*time.Second)
	var problem, outcome string
	p.Eval(t, problemJS, &problem)
	p.Eval(t, `document.querySelector('#outcome').textContent`, &outcome)
	if problem != "" || outcome != "" || s.aborted.Load() != 0 {
		t.Fatalf("problem %q, outcome %q, %d requests aborted", problem, outcome, s.aborted.Load())
	}
}
