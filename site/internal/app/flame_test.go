package app_test

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/browser"
)

// TestFlameShot opens a performance trace (TestLifeDrift's LIFE_DRIFT_TRACE,
// or a window of one cut with bench/drift/crop.py) in the Performance panel
// of Chromium's own DevTools front end and writes a PNG of it: FLAME_TRACE
// is the trace (.json or .json.gz), FLAME_PNG the picture, FLAME_WIDTH and
// FLAME_HEIGHT its size in CSS pixels (default 1800 by 1100). A second
// headless Chromium serves the front end over HTTP on 127.0.0.1, since a
// page may not open devtools://, and the trace goes nowhere else.
//
//	FLAME_TRACE=trace.json FLAME_PNG=flame.png LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestFlameShot
func TestFlameShot(t *testing.T) {
	in, out := os.Getenv("FLAME_TRACE"), os.Getenv("FLAME_PNG")
	if in == "" || out == "" {
		t.Skip("set FLAME_TRACE and FLAME_PNG")
	}
	browser.SkipFirefox(t, "the DevTools front end is Chromium's")
	body, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(in, ".gz") {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if body, err = io.ReadAll(zr); err != nil {
			t.Fatal(err)
		}
	}
	width, height := 1800, 1100
	if n, err := strconv.Atoi(os.Getenv("FLAME_WIDTH")); err == nil {
		width = n
	}
	if n, err := strconv.Atoi(os.Getenv("FLAME_HEIGHT")); err == nil {
		height = n
	}
	// A page may not open devtools://, but a Chromium with a debugging port
	// serves the same front end over HTTP.
	port := freePort(t)
	dir, err := os.MkdirTemp("", "flame")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	helper := exec.Command(browser.Find(t), "--headless=new", "--no-sandbox", "--remote-debugging-port="+strconv.Itoa(port), "--user-data-dir="+dir, "about:blank")
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { helper.Process.Kill(); helper.Wait() }()
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	for i := 0; ; i++ {
		if r, err := http.Get(base + "/devtools/devtools_app.html"); err == nil {
			r.Body.Close()
			break
		}
		if i > 100 {
			t.Fatal("the helper Chromium serves no front end")
		}
		time.Sleep(100 * time.Millisecond)
	}
	b := browser.Start(t)
	p := b.NewPage(t, width, height)
	// The front end fetches the trace from its own origin, and DevTools
	// answers that request with the file.
	traceURL := base + "/devtools/lith-trace.json"
	p.OnEvent("Fetch.requestPaused", func(params json.RawMessage) {
		var e struct{ RequestID string }
		json.Unmarshal(params, &e)
		go p.Call(t, "Fetch.fulfillRequest", map[string]any{"requestId": e.RequestID, "responseCode": 200,
			"responseHeaders": []map[string]string{{"name": "Content-Type", "value": "application/json"}},
			"body":            base64.StdEncoding.EncodeToString(body)})
	})
	p.Call(t, "Fetch.enable", map[string]any{"patterns": []map[string]any{{"urlPattern": traceURL}}})
	p.Call(t, "Page.navigate", map[string]any{"url": base + "/devtools/devtools_app.html?panel=timeline"})
	time.Sleep(5 * time.Second)
	// The panel's own Load profile, with the file fetched from srv.
	var got string
	p.Eval(t, `(async () => {
		const T = await import('./panels/timeline/timeline.js')
		const blob = await (await fetch(`+strconv.Quote(traceURL)+`)).blob()
		await T.TimelinePanel.TimelinePanel.instance().loadFromFile(new File([blob], 'trace.json', {type: 'application/json'}))
		return 'ok'
	})()`, &got)
	time.Sleep(15 * time.Second)
	// Close the keyboard shortcuts the panel shows on its first trace, and
	// the sidebar, so the flame chart has the width.
	click := func(x, y float64) {
		for _, typ := range []string{"mousePressed", "mouseReleased"} {
			p.Call(t, "Input.dispatchMouseEvent", map[string]any{"type": typ, "x": x, "y": y, "button": "left", "clickCount": 1})
		}
		time.Sleep(time.Second)
	}
	click(float64(width-29), 77)
	click(15, 39)
	time.Sleep(2 * time.Second)
	p.Screenshot(t, out, false)
	if errs := p.Errors(); len(errs) > 0 {
		t.Logf("the front end: %v", errs)
	}
}
