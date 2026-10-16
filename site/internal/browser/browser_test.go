package browser_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/browser"
)

// TestDriver checks the driver itself in the engine LITH_BROWSER names:
// navigation, preload scripts, values back from the page, console errors.
//
//	LITH_BROWSER=firefox FIREFOX=... LITH_FIREFOX_NO_SANDBOX=1 go test ./internal/browser
func TestDriver(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!doctype html><title>t</title><p id=p>hi</p><script>window.fromPage = window.early + 1; console.error("boom")</script>`))
	}))
	defer srv.Close()
	b := browser.Start(t)
	p := b.NewPage(t, 800, 600)
	p.AddInitScript(t, `window.early = 41`)
	p.Navigate(t, srv.URL)
	var got struct {
		N    int
		Text string
		List []any
		W    int
	}
	p.Eval(t, `({n: window.fromPage, text: document.getElementById('p').textContent, list: [1, 'a', null, {x: 2}], w: innerWidth})`, &got)
	if got.N != 42 || got.Text != "hi" || len(got.List) != 4 || got.W != 800 {
		t.Errorf("got %+v", got)
	}
	var v float64
	p.Eval(t, `new Promise(r => setTimeout(() => r(1.5), 10))`, &v)
	if v != 1.5 {
		t.Errorf("promise: %v", v)
	}
	p.Wait(t, `document.readyState === 'complete'`, 5*time.Second)
	time.Sleep(200 * time.Millisecond)
	if errs := p.Errors(); len(errs) != 1 || !strings.Contains(errs[0], "boom") {
		t.Errorf("errors: %q", errs)
	}
	t.Logf("%s: %v", p.Engine(), p.Logs())
}
