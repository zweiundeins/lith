package browser

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Firefox runs instead of Chromium when LITH_BROWSER=firefox, driven over
// WebDriver BiDi (Firefox has no DevTools protocol). FIREFOX names the
// binary; LITH_FIREFOX_NO_SANDBOX=1 (or LITH_CHROME_NO_SANDBOX=1) turns
// off its content sandbox where the machine has no user namespaces. Page
// keeps its methods; Call translates the DevTools commands the tests send
// that BiDi has a counterpart for (input, viewport, preload scripts) and
// logs and ignores the rest (CPU and network throttling, media emulation,
// animation speed, performance metrics).

// Engine is "chromium" or "firefox", from LITH_BROWSER.
func Engine() string {
	if strings.EqualFold(os.Getenv("LITH_BROWSER"), "firefox") {
		return "firefox"
	}
	return "chromium"
}

// SkipFirefox skips the test in Firefox, which, or whose driver, lacks
// what it needs; why says what.
func SkipFirefox(t testing.TB, why string) {
	t.Helper()
	if Engine() == "firefox" {
		t.Skip(why)
	}
}

// bidi is a WebDriver BiDi connection.
type bidi struct {
	ws    *wsConn
	mu    sync.Mutex
	next  int
	calls map[int]chan bidiMsg
	err   error
}

type bidiMsg struct {
	Type    string          `json:"type"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   string          `json:"error"`
	Message string          `json:"message"`
}

// firefoxPrefs go into the fresh profile's user.js, on top of the ones the
// remote agent sets itself (remote.prefs.recommended: no updates, no first
// run pages, no slow script dialog).
var firefoxPrefs = map[string]any{
	"browser.shell.checkDefaultBrowser":                  false,
	"datareporting.policy.dataSubmissionEnabled":         false,
	"toolkit.telemetry.reportingpolicy.firstRun":         false,
	"browser.tabs.warnOnClose":                           false,
	"browser.sessionstore.resume_from_crash":             false,
	"dom.max_script_run_time":                            0,
	"dom.max_chrome_script_run_time":                     0,
	"browser.newtabpage.enabled":                         false,
	"browser.startup.homepage_override.mstone":           "ignore",
	"network.captive-portal-service.enabled":             false,
	"network.connectivity-service.enabled":               false,
	"browser.safebrowsing.update.enabled":                false,
	"browser.region.network.url":                         "",
	"app.normandy.enabled":                               false,
	"extensions.update.enabled":                          false,
	"browser.contentblocking.report.lockwise.enabled":    false,
	"browser.discovery.enabled":                          false,
	"media.autoplay.default":                             0,
	"browser.translations.automaticallyPopup":            false,
	"browser.aboutConfig.showWarning":                    false,
	"browser.download.panel.shown":                       true,
	"security.enterprise_roots.enabled":                  false,
	"browser.ml.enable":                                  false,
	"toolkit.startup.max_resumed_crashes":                -1,
	"browser.dom.window.dump.enabled":                    true,
	"devtools.console.stdout.content":                    false,
	"network.http.speculative-parallel-limit":            0,
	"browser.urlbar.suggest.searches":                    false,
	"browser.newtabpage.activity-stream.feeds.telemetry": false,
}

var bidiListening = regexp.MustCompile(`WebDriver BiDi listening on ws://([0-9.:\[\]a-z]+)`)

// startFirefox launches headless Firefox with a fresh profile and opens a
// BiDi session that accepts Caddy's self-signed certificate.
func startFirefox(t testing.TB) *Browser {
	t.Helper()
	bin := os.Getenv("FIREFOX")
	if bin == "" {
		p, err := exec.LookPath("firefox")
		if err != nil {
			t.Skip("no Firefox found (set FIREFOX)")
		}
		bin = p
	}
	profile := t.TempDir()
	var js strings.Builder
	for k, v := range firefoxPrefs {
		b, _ := json.Marshal(v)
		fmt.Fprintf(&js, "user_pref(%q, %s);\n", k, b)
	}
	if err := os.WriteFile(filepath.Join(profile, "user.js"), []byte(js.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--headless", "--no-remote", "--profile", profile, "--remote-debugging-port", "0")
	cmd.Env = append(os.Environ(), "MOZ_CRASHREPORTER_DISABLE=1")
	if os.Getenv("LITH_FIREFOX_NO_SANDBOX") == "1" || os.Getenv("LITH_CHROME_NO_SANDBOX") == "1" {
		cmd.Env = append(cmd.Env, "MOZ_DISABLE_CONTENT_SANDBOX=1")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	b := &Browser{cmd: cmd, stderr: &strings.Builder{}, pages: map[string]*Page{}, engine: "firefox"}
	lw := &lockedWriter{b: b.stderr}
	cmd.Stdout = lw
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	addr := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 1<<16), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			lw.Write([]byte(line + "\n"))
			if m := bidiListening.FindStringSubmatch(line); m != nil {
				select {
				case addr <- m[1]:
				default:
				}
			}
		}
	}()
	kill := func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
	}
	var hostport string
	select {
	case hostport = <-addr:
	case <-time.After(30 * time.Second):
		kill()
		t.Fatalf("firefox did not start its remote agent:\n%s", b.Stderr())
	}
	ws, err := dialWS(hostport, "/session")
	if err != nil {
		kill()
		t.Fatalf("firefox: %v", err)
	}
	b.ff = &bidi{ws: ws, calls: map[int]chan bidiMsg{}}
	go b.readBiDi()
	t.Cleanup(func() {
		b.ff.call("browser.close", map[string]any{}, 5*time.Second)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		// Content processes share the process group.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		ws.Close()
	})
	if _, err := b.ff.call("session.new", map[string]any{"capabilities": map[string]any{"alwaysMatch": map[string]any{
		"acceptInsecureCerts": true, "unhandledPromptBehavior": map[string]any{"default": "ignore"},
	}}}, 30*time.Second); err != nil {
		t.Fatalf("firefox session.new: %v\n%s", err, b.Stderr())
	}
	if _, err := b.ff.call("session.subscribe", map[string]any{"events": []string{
		"log.entryAdded", "browsingContext.load", "network.responseCompleted", "network.fetchError",
	}}, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	return b
}

// readBiDi dispatches Firefox's messages: replies to their calls, events to
// the page of their browsing context.
func (b *Browser) readBiDi() {
	f := b.ff
	for {
		raw, err := f.ws.ReadMessage()
		if err != nil {
			f.mu.Lock()
			f.err = fmt.Errorf("firefox went away: %w", err)
			for id, c := range f.calls {
				close(c)
				delete(f.calls, id)
			}
			f.mu.Unlock()
			return
		}
		var m bidiMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		if m.Type == "success" || m.Type == "error" {
			f.mu.Lock()
			if c, ok := f.calls[m.ID]; ok {
				c <- m
				delete(f.calls, m.ID)
			}
			f.mu.Unlock()
			continue
		}
		var where struct {
			Context string
			Source  struct{ Context string }
		}
		json.Unmarshal(m.Params, &where)
		ctx := where.Context
		if ctx == "" {
			ctx = where.Source.Context
		}
		b.mu.Lock()
		p := b.pages[ctx]
		b.mu.Unlock()
		if p != nil {
			p.event(m.Method, m.Params)
		}
	}
}

func (f *bidi) call(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	f.mu.Lock()
	if f.err != nil {
		f.mu.Unlock()
		return nil, f.err
	}
	f.next++
	id := f.next
	c := make(chan bidiMsg, 1)
	f.calls[id] = c
	msg, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	err := f.ws.WriteText(msg)
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case m, ok := <-c:
		if !ok {
			return nil, f.err
		}
		if m.Type == "error" {
			return nil, fmt.Errorf("%s: %s: %s", method, m.Error, m.Message)
		}
		return m.Result, nil
	case <-time.After(timeout):
		f.mu.Lock()
		delete(f.calls, id)
		f.mu.Unlock()
		return nil, fmt.Errorf("%s: no reply in %v", method, timeout)
	}
}

// newFirefoxPage opens a window in a user context of its own (its cookies).
func (b *Browser) newFirefoxPage(t testing.TB, width, height int) *Page {
	t.Helper()
	res, err := b.ff.call("browser.createUserContext", nil, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var uc struct{ UserContext string }
	json.Unmarshal(res, &uc)
	// A window of its own: a tab behind another is hidden, and Datastar
	// closes a hidden page's stream.
	res, err = b.ff.call("browsingContext.create", map[string]any{"type": "window", "userContext": uc.UserContext}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var bc struct{ Context string }
	json.Unmarshal(res, &bc)
	p := &Page{b: b, session: bc.Context, target: bc.Context, loaded: make(chan struct{}, 1)}
	b.mu.Lock()
	b.pages[p.session] = p
	b.mu.Unlock()
	p.viewport(t, width, height, 1)
	return p
}

func (p *Page) viewport(t testing.TB, width, height int, scale float64) {
	t.Helper()
	p.BiDi(t, "browsingContext.setViewport", map[string]any{"context": p.session, "viewport": map[string]any{"width": width, "height": height}, "devicePixelRatio": scale})
}

// BiDi sends a WebDriver BiDi command (Firefox only).
func (p *Page) BiDi(t testing.TB, method string, params any) json.RawMessage {
	t.Helper()
	res, err := p.b.ff.call(method, params, 60*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Engine is the page's browser: "chromium" or "firefox".
func (p *Page) Engine() string { return p.b.engine }

// AddInitScript runs src in every document the page loads, before the
// page's own scripts.
func (p *Page) AddInitScript(t testing.TB, src string) {
	t.Helper()
	if p.b.ff != nil {
		p.BiDi(t, "script.addPreloadScript", map[string]any{"functionDeclaration": "() => {\n" + src + "\n}", "contexts": []string{p.session}})
		return
	}
	p.Call(t, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": src})
}

// focus brings the page's window to the front before the test acts in it.
// Headless Chromium gives every page focus; in Firefox only the window in
// front has it, and a focus() in another sends no focus events.
func (p *Page) focus() error {
	p.b.mu.Lock()
	front := p.b.front == p.session
	p.b.front = p.session
	p.b.mu.Unlock()
	if front {
		return nil
	}
	_, err := p.b.ff.call("browsingContext.activate", map[string]any{"context": p.session}, 30*time.Second)
	return err
}

func (p *Page) evalFirefox(expr string, out any, timeout time.Duration) error {
	if err := p.focus(); err != nil {
		return err
	}
	res, err := p.b.ff.call("script.evaluate", map[string]any{
		"expression": expr, "target": map[string]any{"context": p.session}, "awaitPromise": true,
		"resultOwnership": "none", "userActivation": true,
	}, timeout)
	if err != nil {
		return err
	}
	var r struct {
		Type             string
		Result           json.RawMessage
		ExceptionDetails struct {
			Text string
		}
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return err
	}
	if r.Type == "exception" {
		return fmt.Errorf("%s: %s", expr, r.ExceptionDetails.Text)
	}
	if out == nil {
		return nil
	}
	b, err := json.Marshal(remoteValue(r.Result))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// remoteValue turns a BiDi RemoteValue into what JSON.stringify would give.
func remoteValue(raw json.RawMessage) any {
	var v struct {
		Type  string
		Value json.RawMessage
	}
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	switch v.Type {
	case "string", "boolean", "date":
		var x any
		json.Unmarshal(v.Value, &x)
		return x
	case "number":
		var x any
		json.Unmarshal(v.Value, &x)
		if s, ok := x.(string); ok {
			if s == "-0" {
				return 0
			}
			return nil // NaN, Infinity
		}
		return x
	case "bigint":
		var s string
		json.Unmarshal(v.Value, &s)
		return json.Number(s)
	case "array", "set":
		var items []json.RawMessage
		json.Unmarshal(v.Value, &items)
		out := make([]any, len(items))
		for i, it := range items {
			out[i] = remoteValue(it)
		}
		return out
	case "object", "map":
		var pairs [][2]json.RawMessage
		json.Unmarshal(v.Value, &pairs)
		out := map[string]any{}
		for _, kv := range pairs {
			var k string
			if json.Unmarshal(kv[0], &k) != nil {
				k = fmt.Sprint(remoteValue(kv[0]))
			}
			out[k] = remoteValue(kv[1])
		}
		return out
	}
	return nil // undefined, null, functions, nodes
}

// firefoxEvent records the page's console errors, uncaught exceptions and
// failed loads, and its load events.
func (p *Page) firefoxEvent(method string, params json.RawMessage) {
	switch method {
	case "browsingContext.load":
		select {
		case p.loaded <- struct{}{}:
		default:
		}
	case "log.entryAdded":
		var e struct {
			Type, Level, Text, Method string
		}
		json.Unmarshal(params, &e)
		text := e.Level + " " + e.Type + ": " + e.Text
		if e.Type == "console" {
			text = "console." + e.Method + ": " + e.Text
		}
		p.mu.Lock()
		p.logs = append(p.logs, text)
		if e.Level == "error" {
			if e.Type == "javascript" {
				text = "exception: " + e.Text
			}
			p.errs = append(p.errs, text)
		}
		p.mu.Unlock()
	case "network.responseCompleted":
		var e struct {
			Request  struct{ URL string }
			Response struct {
				URL    string
				Status int
			}
		}
		json.Unmarshal(params, &e)
		if e.Response.Status >= 400 {
			p.mu.Lock()
			p.errs = append(p.errs, fmt.Sprintf("error network: Failed to load resource: the server responded with a status of %d %s", e.Response.Status, e.Request.URL))
			p.mu.Unlock()
		}
	case "network.fetchError":
		var e struct {
			ErrorText string
			Request   struct{ URL string }
		}
		json.Unmarshal(params, &e)
		// Datastar aborts a stream it opens again, and navigation ends them.
		if strings.Contains(e.ErrorText, "ABORT") {
			return
		}
		p.mu.Lock()
		p.errs = append(p.errs, "error network: "+e.ErrorText+" "+e.Request.URL)
		p.mu.Unlock()
	}
}

var keyValues = map[string]string{
	"Escape": "", "Enter": "", "Tab": "", "Backspace": "", "Delete": "",
	"ArrowLeft": "", "ArrowUp": "", "ArrowRight": "", "ArrowDown": "",
	"Home": "", "End": "", "PageUp": "", "PageDown": "", "Shift": "",
	"Control": "", "Alt": "", "Meta": "", "Space": " ", " ": " ",
}

func keyValue(key string) string {
	if v, ok := keyValues[key]; ok {
		return v
	}
	return key
}

// callFirefox carries out the DevTools commands the tests send.
func (p *Page) callFirefox(t testing.TB, method string, params any) json.RawMessage {
	t.Helper()
	var m map[string]any
	b, _ := json.Marshal(params)
	json.Unmarshal(b, &m)
	num := func(k string) float64 { f, _ := m[k].(float64); return f }
	str := func(k string) string { s, _ := m[k].(string); return s }
	act := func(source map[string]any) json.RawMessage {
		if err := p.focus(); err != nil {
			t.Fatal(err)
		}
		return p.BiDi(t, "input.performActions", map[string]any{"context": p.session, "actions": []any{source}})
	}
	button := func() int {
		switch str("button") {
		case "middle":
			return 1
		case "right":
			return 2
		}
		return 0
	}
	switch method {
	case "Input.dispatchMouseEvent":
		x, y := math.Round(num("x")), math.Round(num("y"))
		move := map[string]any{"type": "pointerMove", "x": x, "y": y, "origin": "viewport", "duration": 0}
		var acts []any
		switch str("type") {
		case "mouseMoved":
			acts = []any{move}
		case "mousePressed":
			acts = []any{move, map[string]any{"type": "pointerDown", "button": button()}}
		case "mouseReleased":
			acts = []any{move, map[string]any{"type": "pointerUp", "button": button()}}
		case "mouseWheel":
			return act(map[string]any{"type": "wheel", "id": "wheel", "actions": []any{map[string]any{"type": "scroll", "x": x, "y": y, "deltaX": num("deltaX"), "deltaY": num("deltaY"), "origin": "viewport"}}})
		}
		return act(map[string]any{"type": "pointer", "id": "mouse", "parameters": map[string]any{"pointerType": "mouse"}, "actions": acts})
	case "Input.dispatchTouchEvent":
		pts, _ := m["touchPoints"].([]any)
		var acts []any
		at := func() map[string]any {
			pt, _ := pts[0].(map[string]any)
			x, _ := pt["x"].(float64)
			y, _ := pt["y"].(float64)
			return map[string]any{"type": "pointerMove", "x": math.Round(x), "y": math.Round(y), "origin": "viewport", "duration": 0}
		}
		switch str("type") {
		case "touchStart":
			acts = []any{at(), map[string]any{"type": "pointerDown", "button": 0}}
		case "touchMove":
			acts = []any{at()}
		case "touchEnd", "touchCancel":
			acts = []any{map[string]any{"type": "pointerUp", "button": 0}}
		}
		if len(pts) > 1 {
			t.Logf("firefox: only the first of %d touch points", len(pts))
		}
		return act(map[string]any{"type": "pointer", "id": "touch", "parameters": map[string]any{"pointerType": "touch"}, "actions": acts})
	case "Input.dispatchKeyEvent":
		v := keyValue(str("key"))
		switch str("type") {
		case "rawKeyDown", "keyDown":
			return act(map[string]any{"type": "key", "id": "keyboard", "actions": []any{map[string]any{"type": "keyDown", "value": v}}})
		case "keyUp":
			return act(map[string]any{"type": "key", "id": "keyboard", "actions": []any{map[string]any{"type": "keyUp", "value": v}}})
		}
		return json.RawMessage(`{}`)
	case "Emulation.setDeviceMetricsOverride":
		scale := num("deviceScaleFactor")
		if scale == 0 {
			scale = 1
		}
		p.viewport(t, int(num("width")), int(num("height")), scale)
		return json.RawMessage(`{}`)
	case "Page.addScriptToEvaluateOnNewDocument":
		p.AddInitScript(t, str("source"))
		return json.RawMessage(`{}`)
	case "Security.setIgnoreCertificateErrors", "Network.enable", "Page.enable", "Runtime.enable", "Log.enable":
		return json.RawMessage(`{}`) // the session accepts insecure certificates; events come by subscription
	}
	p.b.warnOnce(t, method)
	return json.RawMessage(`{}`)
}

func (b *Browser) warnOnce(t testing.TB, method string) {
	b.mu.Lock()
	if b.warned == nil {
		b.warned = map[string]bool{}
	}
	seen := b.warned[method]
	b.warned[method] = true
	b.mu.Unlock()
	if !seen {
		t.Logf("firefox: %s has no BiDi counterpart here; ignored", method)
	}
}

func (p *Page) screenshotFirefox(t testing.TB, path string, clip map[string]any, document bool) {
	t.Helper()
	params := map[string]any{"context": p.session, "origin": "viewport"}
	if document || clip != nil {
		params["origin"] = "document"
	}
	if clip != nil {
		params["clip"] = clip
	}
	var shot struct{ Data string }
	json.Unmarshal(p.BiDi(t, "browsingContext.captureScreenshot", params), &shot)
	b, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
