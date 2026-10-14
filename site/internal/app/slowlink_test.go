package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zweiundeins/lith/edge"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
)

// TestSlowLinkStream reads the Game of Life on two streams at once: one
// with net=slow, as a page on a slow link opens it, gets the board four
// times a second at most, the other every generation. Documents ask for
// Chromium's network hints, and a stream whose request carries a slow one
// is slow too.
func TestSlowLinkStream(t *testing.T) {
	_, base := apptest.Start(t)
	c := client(t)
	res, err := c.Get(base + "/demos/life")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if got := res.Header.Get("Accept-CH"); got != edge.NetworkHints {
		t.Errorf("Accept-CH: %q", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	count := func(url string, header http.Header) func() int {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header = header
		req.Header.Set("Accept-Encoding", "identity")
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		n := 0
		go func() {
			defer res.Body.Close()
			buf := make([]byte, 64<<10)
			for {
				k, err := res.Body.Read(buf)
				mu.Lock()
				n += strings.Count(string(buf[:k]), `id="life-board"`)
				mu.Unlock()
				if err != nil {
					return
				}
			}
		}()
		return func() int {
			mu.Lock()
			defer mu.Unlock()
			return n
		}
	}
	slow := count(base+"/demos/life/stream?tab=slow1&net=slow", http.Header{})
	hinted := count(base+"/demos/life/stream?tab=slow2", http.Header{"Ect": {"3g"}})
	fast := count(base+"/demos/life/stream?tab=fast1", http.Header{})
	time.Sleep(3 * time.Second)
	s, h, f := slow(), hinted(), fast()
	// A frame a generation, ten a second; the first frame and then four
	// a second on a slow link.
	if s > 14 || h > 14 || f < 20 || s < 4 || h < 4 {
		t.Errorf("in 3 s, the board's frames: %d with net=slow, %d with ECT 3g, %d without", s, h, f)
	}
}

// TestSlowLinkPage opens the stream inspector in Chromium on a link that
// DevTools throttles as its Slow 4G preset: the page finds it slow, its
// stream asks with net=slow while the page's own URL stays as it was, and
// the stream reports zstd at its default level and four frames a second at
// most. Without navigator.connection, as in Firefox and Safari, Navigation
// Timing tells the same; where the page's measurement says nothing,
// Chromium's network hints, which the document asked for, still do.
// Unthrottled, the page is fast.
//
//	LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestSlowLinkPage
func TestSlowLinkPage(t *testing.T) {
	_, base := apptest.Start(t)
	b := browser.Start(t)
	const (
		noNetInfo = `Object.defineProperty(Navigator.prototype, 'connection', {get: () => undefined})`
		says4G    = `Object.defineProperty(Navigator.prototype, 'connection', {get: () => ({effectiveType: '4g', saveData: false})})`
	)
	for _, c := range []struct {
		name          string
		throttle      bool
		script        string
		netSlow, tier bool // net=slow in the stream's URL; the denser tier
	}{
		{"unthrottled", false, "", false, false},
		{"Slow 4G", true, "", true, true},
		{"Slow 4G without navigator.connection", true, noNetInfo, true, true},
		{"unthrottled without navigator.connection", false, noNetInfo, false, false},
		{"Slow 4G with navigator.connection saying 4g", true, says4G, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := b.NewPage(t, 1280, 900)
			defer p.Close(t)
			var mu sync.Mutex
			var streams []string
			p.OnEvent("Network.requestWillBeSent", func(params json.RawMessage) {
				var e struct{ Request struct{ URL string } }
				json.Unmarshal(params, &e)
				if strings.Contains(e.Request.URL, "/stream?") {
					mu.Lock()
					streams = append(streams, e.Request.URL)
					mu.Unlock()
				}
			})
			p.Call(t, "Network.enable", nil)
			if c.throttle {
				p.Call(t, "Network.emulateNetworkConditions", slow4G)
			}
			if c.script != "" {
				p.Call(t, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": c.script})
			}
			p.Navigate(t, base+"/demos/stream")
			p.Wait(t, `document.querySelector('.live-badge.is-live')`, 30*time.Second)
			want := "zstd 2^"
			if c.tier {
				want = "zstd default 2^"
			}
			p.Wait(t, `document.querySelector('.sx-pairs code').textContent.startsWith('`+want+`')`, 30*time.Second)
			var search string
			p.Eval(t, `location.search`, &search)
			mu.Lock()
			got := strings.Join(streams, " ")
			mu.Unlock()
			if search != "" || strings.Contains(got, "net=slow") != c.netSlow {
				t.Errorf("the page's query %q, its stream %s", search, got)
			}
			if c.tier {
				time.Sleep(2 * time.Second)
				var fps float64
				p.Eval(t, `Number(document.querySelector('[data-text="$_stream.fps"]').textContent)`, &fps)
				if fps > 4.5 {
					t.Errorf("%.1f frames a second", fps)
				}
			}
			if errs := p.Errors(); len(errs) > 0 {
				t.Errorf("the page: %v", errs)
			}
		})
	}
}
