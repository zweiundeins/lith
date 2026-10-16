// Package browser drives headless Chromium for the site's tests, over the
// DevTools protocol on a pipe (--remote-debugging-pipe): no websocket, no
// dependency. A test opens pages, runs JavaScript in them, waits for
// conditions, takes screenshots, and fails on what the console reports.
//
// Chromium runs with its sandbox, or without it where the machine has none
// and LITH_CHROME_NO_SANDBOX=1 (as root it never has one). A test that finds
// no Chromium is skipped; CHROME names the binary. LITH_BROWSER=firefox
// runs the same tests in Firefox over WebDriver BiDi (firefox.go).
package browser

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// Browser is a running Chromium.
type Browser struct {
	cmd    *exec.Cmd
	w      io.WriteCloser
	r      *bufio.Reader
	stderr *strings.Builder

	mu    sync.Mutex
	next  int
	calls map[int]chan message
	pages map[string]*Page
	err   error

	engine string          // "chromium" or "firefox"
	ff     *bidi           // Firefox's WebDriver BiDi connection
	warned map[string]bool // DevTools commands Firefox ignored, logged once
	front  string          // the Firefox window in front
}

type message struct {
	ID        int             `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Find returns the Chromium binary, or skips the test.
func Find(t testing.TB) string {
	t.Helper()
	if c := os.Getenv("CHROME"); c != "" {
		return c
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("no Chromium found (set CHROME)")
	return ""
}

// Start launches headless Chromium for the test, or Firefox when
// LITH_BROWSER=firefox; it ends with the test.
func Start(t testing.TB) *Browser {
	t.Helper()
	if testing.Short() {
		t.Skip("starts a browser")
	}
	if Engine() == "firefox" {
		return startFirefox(t)
	}
	chrome := Find(t)
	args := []string{
		"--headless=new", "--remote-debugging-pipe", "--user-data-dir=" + t.TempDir(),
		"--no-first-run", "--no-default-browser-check", "--disable-gpu", "--hide-scrollbars",
		"--disable-background-networking", "--disable-component-update", "--disable-sync",
		"--font-render-hinting=none", "about:blank",
	}
	if os.Getenv("LITH_CHROME_NO_SANDBOX") == "1" || os.Geteuid() == 0 {
		args = append([]string{"--no-sandbox"}, args...)
	}
	// Chromium reads the protocol on fd 3 and writes it on fd 4.
	toChrome, ourW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ourR, fromChrome, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(chrome, args...)
	cmd.ExtraFiles = []*os.File{toChrome, fromChrome}
	b := &Browser{cmd: cmd, w: ourW, r: bufio.NewReaderSize(ourR, 1<<20), stderr: &strings.Builder{}, calls: map[int]chan message{}, pages: map[string]*Page{}, engine: "chromium"}
	cmd.Stderr = &lockedWriter{b: b.stderr}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	toChrome.Close()
	fromChrome.Close()
	go b.read()
	t.Cleanup(func() {
		b.call("", "Browser.close", nil)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
		}
		ourW.Close()
		ourR.Close()
	})
	if _, err := b.call("", "Browser.getVersion", nil); err != nil {
		if strings.Contains(b.Stderr(), "No usable sandbox") {
			t.Skip("Chromium has no usable sandbox here; set LITH_CHROME_NO_SANDBOX=1")
		}
		t.Fatalf("chromium: %v\n%s", err, b.Stderr())
	}
	return b
}

type lockedWriter struct {
	mu sync.Mutex
	b  *strings.Builder
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// Stderr is what the browser wrote to its standard error (Firefox: and to
// its standard output).
func (b *Browser) Stderr() string {
	lw, ok := b.cmd.Stderr.(*lockedWriter)
	if !ok {
		lw, ok = b.cmd.Stdout.(*lockedWriter)
	}
	if ok {
		lw.mu.Lock()
		defer lw.mu.Unlock()
	}
	return b.stderr.String()
}

// read dispatches Chromium's messages: replies to their calls, events to
// their pages.
func (b *Browser) read() {
	for {
		line, err := b.r.ReadBytes(0)
		if err != nil {
			b.mu.Lock()
			b.err = fmt.Errorf("chromium went away: %w", err)
			for id, c := range b.calls {
				close(c)
				delete(b.calls, id)
			}
			b.mu.Unlock()
			return
		}
		var m message
		if err := json.Unmarshal(line[:len(line)-1], &m); err != nil {
			continue
		}
		b.mu.Lock()
		if m.ID != 0 {
			if c, ok := b.calls[m.ID]; ok {
				c <- m
				delete(b.calls, m.ID)
			}
			b.mu.Unlock()
			continue
		}
		p := b.pages[m.SessionID]
		b.mu.Unlock()
		if p != nil {
			p.event(m.Method, m.Params)
		}
	}
}

// call sends a command and waits up to 30 s for its reply.
func (b *Browser) call(session, method string, params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	if params == nil {
		raw = nil
	}
	b.mu.Lock()
	if b.err != nil {
		b.mu.Unlock()
		return nil, b.err
	}
	b.next++
	id := b.next
	c := make(chan message, 1)
	b.calls[id] = c
	msg, _ := json.Marshal(message{ID: id, Method: method, Params: raw, SessionID: session})
	_, err = b.w.Write(append(msg, 0))
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case m, ok := <-c:
		if !ok {
			return nil, b.err
		}
		if m.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, m.Error.Message)
		}
		return m.Result, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("%s: no reply in 30 s", method)
	}
}

// Page is a tab.
type Page struct {
	b       *Browser
	session string
	target  string

	mu     sync.Mutex
	errs   []string
	logs   []string
	loaded chan struct{}
	on     map[string][]func(json.RawMessage) // OnEvent's handlers by method
}

// NewPage opens a tab of width×height CSS pixels, in a browser context of
// its own: its cookies, and so its session, are its alone.
func (b *Browser) NewPage(t testing.TB, width, height int) *Page {
	t.Helper()
	if b.ff != nil {
		return b.newFirefoxPage(t, width, height)
	}
	return b.newChromiumPage(t, width, height)
}

func (b *Browser) newChromiumPage(t testing.TB, width, height int) *Page {
	t.Helper()
	res, err := b.call("", "Target.createBrowserContext", map[string]any{"disposeOnDetach": true})
	if err != nil {
		t.Fatal(err)
	}
	var bc struct{ BrowserContextID string }
	json.Unmarshal(res, &bc)
	res, err = b.call("", "Target.createTarget", map[string]any{"url": "about:blank", "browserContextId": bc.BrowserContextID})
	if err != nil {
		t.Fatal(err)
	}
	var target struct{ TargetID string }
	json.Unmarshal(res, &target)
	res, err = b.call("", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true})
	if err != nil {
		t.Fatal(err)
	}
	var att struct{ SessionID string }
	json.Unmarshal(res, &att)
	p := &Page{b: b, session: att.SessionID, target: target.TargetID, loaded: make(chan struct{}, 1)}
	b.mu.Lock()
	b.pages[p.session] = p
	b.mu.Unlock()
	for _, m := range []string{"Page.enable", "Runtime.enable", "Log.enable"} {
		if _, err := b.call(p.session, m, nil); err != nil {
			t.Fatal(err)
		}
	}
	p.Call(t, "Emulation.setDeviceMetricsOverride", map[string]any{"width": width, "height": height, "deviceScaleFactor": 1, "mobile": false})
	return p
}

// Call sends a DevTools command to the page.
func (p *Page) Call(t testing.TB, method string, params any) json.RawMessage {
	t.Helper()
	if p.b.ff != nil {
		return p.callFirefox(t, method, params)
	}
	res, err := p.b.call(p.session, method, params)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// OnEvent calls f with the params of each of the page's events named
// method, such as "Network.dataReceived" once the test has called
// Network.enable. f runs on the goroutine that reads from Chromium: it
// must return quickly and must not call the page.
func (p *Page) OnEvent(method string, f func(params json.RawMessage)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.on == nil {
		p.on = map[string][]func(json.RawMessage){}
	}
	p.on[method] = append(p.on[method], f)
}

func (p *Page) event(method string, params json.RawMessage) {
	p.mu.Lock()
	on := p.on[method]
	p.mu.Unlock()
	for _, f := range on {
		f(params)
	}
	if p.b.ff != nil {
		p.firefoxEvent(method, params)
		return
	}
	switch method {
	case "Page.loadEventFired":
		select {
		case p.loaded <- struct{}{}:
		default:
		}
	case "Runtime.consoleAPICalled":
		var e struct {
			Type string
			Args []struct {
				Value       any
				Description string
			}
		}
		json.Unmarshal(params, &e)
		var parts []string
		for _, a := range e.Args {
			if a.Description != "" {
				parts = append(parts, a.Description)
			} else {
				parts = append(parts, fmt.Sprint(a.Value))
			}
		}
		text := "console." + e.Type + ": " + strings.Join(parts, " ")
		p.mu.Lock()
		p.logs = append(p.logs, text)
		if e.Type == "error" || e.Type == "assert" {
			p.errs = append(p.errs, text)
		}
		p.mu.Unlock()
	case "Runtime.exceptionThrown":
		var e struct {
			ExceptionDetails struct {
				Text      string
				URL       string
				Exception struct{ Description string }
			}
		}
		json.Unmarshal(params, &e)
		d := e.ExceptionDetails
		p.mu.Lock()
		p.errs = append(p.errs, "exception: "+d.Text+" "+d.Exception.Description+" "+d.URL)
		p.mu.Unlock()
	case "Log.entryAdded":
		var e struct {
			Entry struct{ Level, Source, Text, URL string }
		}
		json.Unmarshal(params, &e)
		text := e.Entry.Level + " " + e.Entry.Source + ": " + e.Entry.Text + " " + e.Entry.URL
		p.mu.Lock()
		p.logs = append(p.logs, text)
		if e.Entry.Level == "error" {
			p.errs = append(p.errs, text)
		}
		p.mu.Unlock()
	}
}

// Errors are the console errors, uncaught exceptions and failed loads the
// page has reported so far.
func (p *Page) Errors() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.errs...)
}

// Logs are everything the console and the browser's log reported.
func (p *Page) Logs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.logs...)
}

// Navigate loads url and waits for its load event.
func (p *Page) Navigate(t testing.TB, url string) {
	t.Helper()
	select {
	case <-p.loaded:
	default:
	}
	if p.b.ff != nil {
		p.BiDi(t, "browsingContext.navigate", map[string]any{"context": p.session, "url": url, "wait": "none"})
		p.WaitLoad(t)
		return
	}
	res := p.Call(t, "Page.navigate", map[string]any{"url": url})
	var nav struct{ ErrorText string }
	json.Unmarshal(res, &nav)
	if nav.ErrorText != "" {
		t.Fatalf("navigate %s: %s", url, nav.ErrorText)
	}
	p.WaitLoad(t)
}

// WaitLoad waits for the page's next load event.
func (p *Page) WaitLoad(t testing.TB) {
	t.Helper()
	select {
	case <-p.loaded:
	case <-time.After(20 * time.Second):
		t.Fatalf("no load event in 20 s; errors: %v", p.Errors())
	}
}

// TryCall is Call that returns its error, for Chromium only.
func (p *Page) TryCall(method string, params any) (json.RawMessage, error) {
	return p.b.call(p.session, method, params)
}

// TryEval is Eval that returns its error, such as no reply in 30 s from
// a page whose main thread is busy for longer.
func (p *Page) TryEval(expr string, out any) error { return p.eval(expr, out) }

// Eval runs a JavaScript expression, awaits it if it is a promise, and
// unmarshals its value into out, if out is not nil.
func (p *Page) Eval(t testing.TB, expr string, out any) {
	t.Helper()
	if err := p.eval(expr, out); err != nil {
		t.Fatal(err)
	}
}

func (p *Page) eval(expr string, out any) error {
	if p.b.ff != nil {
		return p.evalFirefox(expr, out, 60*time.Second)
	}
	res, err := p.b.call(p.session, "Runtime.evaluate", map[string]any{"expression": expr, "awaitPromise": true, "returnByValue": true, "userGesture": true})
	if err != nil {
		return err
	}
	var r struct {
		Result struct {
			Value json.RawMessage
		}
		ExceptionDetails *struct {
			Text      string
			Exception struct{ Description string }
		}
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return err
	}
	if r.ExceptionDetails != nil {
		return fmt.Errorf("%s: %s %s", expr, r.ExceptionDetails.Text, r.ExceptionDetails.Exception.Description)
	}
	if out != nil && len(r.Result.Value) > 0 {
		return json.Unmarshal(r.Result.Value, out)
	}
	return nil
}

// Wait polls a JavaScript expression until it is true, for up to timeout.
func (p *Page) Wait(t testing.TB, expr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		var ok bool
		if last = p.eval("Boolean("+expr+")", &ok); last == nil && ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("waited %v for %s (last error: %v); page errors: %v", timeout, expr, last, p.Errors())
}

// Screenshot writes a PNG of the viewport, or of the whole page.
func (p *Page) Screenshot(t testing.TB, path string, fullPage bool) {
	t.Helper()
	if p.b.ff != nil {
		p.screenshotFirefox(t, path, nil, fullPage)
		return
	}
	params := map[string]any{"format": "png"}
	if fullPage {
		res := p.Call(t, "Page.getLayoutMetrics", nil)
		var m struct {
			CSSContentSize struct{ Width, Height float64 }
		}
		json.Unmarshal(res, &m)
		params["captureBeyondViewport"] = true
		params["clip"] = map[string]any{"x": 0, "y": 0, "width": m.CSSContentSize.Width, "height": m.CSSContentSize.Height, "scale": 1}
	}
	res := p.Call(t, "Page.captureScreenshot", params)
	var shot struct{ Data string }
	json.Unmarshal(res, &shot)
	b, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Close closes the tab.
func (p *Page) Close(t testing.TB) {
	t.Helper()
	if p.b.ff != nil {
		if _, err := p.b.ff.call("browsingContext.close", map[string]any{"context": p.session}, 30*time.Second); err != nil {
			t.Log(err)
		}
		return
	}
	if _, err := p.b.call("", "Target.closeTarget", map[string]any{"targetId": p.target}); err != nil && !errors.Is(err, io.EOF) {
		t.Log(err)
	}
}

// ShotElement writes a PNG of the first element that matches selector,
// scrolled into view, at scale (2 for a closer look).
func (p *Page) ShotElement(t testing.TB, selector, path string, scale float64) {
	t.Helper()
	var r struct{ X, Y, W, H float64 }
	p.Eval(t, `(() => { const e = document.querySelector(`+jsQuote(selector)+`); e.scrollIntoView({block: 'center'}); const b = e.getBoundingClientRect(); return {x: b.left + scrollX, y: b.top + scrollY, w: b.width, h: b.height} })()`, &r)
	if p.b.ff != nil {
		p.screenshotFirefox(t, path, map[string]any{"type": "box", "x": r.X, "y": r.Y, "width": r.W, "height": r.H}, true)
		return
	}
	res := p.Call(t, "Page.captureScreenshot", map[string]any{"format": "png", "captureBeyondViewport": true, "clip": map[string]any{"x": r.X, "y": r.Y, "width": r.W, "height": r.H, "scale": scale}})
	var shot struct{ Data string }
	json.Unmarshal(res, &shot)
	b, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func jsQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Press sends a key press (down and up) to the focused element: "Escape",
// "Enter", "Tab".
func (p *Page) Press(t testing.TB, key string) {
	t.Helper()
	codes := map[string]int{"Escape": 27, "Enter": 13, "Tab": 9}
	if p.b.ff != nil {
		v := keyValue(key)
		if err := p.focus(); err != nil {
			t.Fatal(err)
		}
		p.BiDi(t, "input.performActions", map[string]any{"context": p.session, "actions": []any{map[string]any{"type": "key", "id": "keyboard", "actions": []any{map[string]any{"type": "keyDown", "value": v}, map[string]any{"type": "keyUp", "value": v}}}}})
		return
	}
	for _, typ := range []string{"rawKeyDown", "keyUp"} {
		p.Call(t, "Input.dispatchKeyEvent", map[string]any{"type": typ, "key": key, "code": key, "windowsVirtualKeyCode": codes[key]})
	}
}
