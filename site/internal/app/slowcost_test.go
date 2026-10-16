package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
)

// DevTools' network presets, as Network.emulateNetworkConditions takes
// them: latency in ms, throughput in bytes a second.
var (
	slow4G = map[string]any{"offline": false, "latency": 150 * 3.75, "downloadThroughput": 1.6e6 / 8 * 0.9, "uploadThroughput": 750e3 / 8 * 0.9}
	threeG = map[string]any{"offline": false, "latency": 400 * 5, "downloadThroughput": 500e3 / 8 * 0.8, "uploadThroughput": 500e3 / 8 * 0.8}
)

// TestSlowLinkCost measures a tab of the Game of Life, a fresh soup, and
// of the landing page, with five cursors moving behind its title, on links
// that DevTools throttles: unthrottled, Slow 4G and 3G, the last two with
// the CPU four times slower, as on a phone. Over 10 s it counts the bytes
// that arrive on the tab's stream, the frames the hub sent it and those
// the page applied, the main thread's time in tasks, its long tasks, and
// how long a key press waits: a page that answers within 200 ms stays
// interactive. It only reports.
//
//	SLOW_LINK_COST=1 LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestSlowLinkCost -v -timeout 30m
func TestSlowLinkCost(t *testing.T) {
	if os.Getenv("SLOW_LINK_COST") == "" {
		t.Skip("set SLOW_LINK_COST=1 to measure")
	}
	a, base := apptest.Start(t)
	b := browser.Start(t)
	defer crowd(t, a, 5)() // the cursors behind the landing page's title
	links := []struct {
		name string
		net  map[string]any
		cpu  float64
	}{
		{"unthrottled", nil, 1},
		{"Slow 4G, CPU 4x", slow4G, 4},
		{"3G, CPU 4x", threeG, 4},
	}
	pages := []struct {
		name, path, applied string
	}{
		{"life", "/demos/life", "#life-stats"},
		{"landing", "/", "#cursor-party-cursors"},
	}
	t.Logf("| Page | Link | Stream | Stream KB/s | Frames sent/s | Applied/s | Main thread busy | Long tasks | Longest task | Slowest key press |")
	for _, pg := range pages {
		for _, l := range links {
			p := b.NewPage(t, 1440, 900)
			var mu sync.Mutex
			streams := map[string]string{}
			var received atomic.Int64
			p.OnEvent("Network.requestWillBeSent", func(params json.RawMessage) {
				var e struct {
					RequestID string
					Request   struct {
						URL     string
						Headers map[string]string
					}
				}
				json.Unmarshal(params, &e)
				if strings.Contains(e.Request.URL, "/stream?") {
					mu.Lock()
					streams[e.RequestID] = e.Request.URL + saveData(e.Request.Headers)
					mu.Unlock()
				}
			})
			p.OnEvent("Network.dataReceived", func(params json.RawMessage) {
				var e struct {
					RequestID         string
					EncodedDataLength int64
				}
				json.Unmarshal(params, &e)
				mu.Lock()
				_, ok := streams[e.RequestID]
				mu.Unlock()
				if ok {
					received.Add(e.EncodedDataLength)
				}
			})
			p.Call(t, "Network.enable", nil)
			if l.net != nil {
				p.Call(t, "Network.emulateNetworkConditions", l.net)
			}
			if l.cpu > 1 {
				p.Call(t, "Emulation.setCPUThrottlingRate", map[string]any{"rate": l.cpu})
			}
			p.Navigate(t, base+pg.path)
			p.Wait(t, `document.querySelector('`+pg.applied+`') !== null`, 30*time.Second)
			waitFor(t, "the stream's first bytes", func() bool { return received.Load() > 0 })
			if pg.name == "life" {
				if err := a.Writer().Do(context.Background(), writer.Func(func(tx *writer.Tx) error {
					_, err := tx.Exec(`UPDATE life_board SET cells = zeroblob(4000), quiet = 1`)
					return err
				})); err != nil {
					t.Fatal(err)
				}
			}
			time.Sleep(3 * time.Second)
			p.Eval(t, `window.applied_ = 0; new MutationObserver(() => window.applied_++).observe(document.querySelector('`+pg.applied+`'), {subtree: true, childList: true, attributes: true, characterData: true});
				window.long_ = []; new PerformanceObserver(l => window.long_.push(...l.getEntries().map(e => e.duration))).observe({type: 'longtask'});
				window.keys_ = []; new PerformanceObserver(l => window.keys_.push(...l.getEntries().filter(e => e.name === 'keydown').map(e => e.duration))).observe({type: 'event', durationThreshold: 16}); 0`, nil)
			p.Call(t, "Performance.enable", map[string]any{"timeDomain": "timeTicks"})
			metrics := func() map[string]float64 {
				var r struct {
					Metrics []struct {
						Name  string
						Value float64
					}
				}
				json.Unmarshal(p.Call(t, "Performance.getMetrics", nil), &r)
				m := map[string]float64{}
				for _, x := range r.Metrics {
					m[x.Name] = x.Value
				}
				return m
			}
			const window = 10 * time.Second
			before, bytes0, sent0, start := metrics(), received.Load(), a.Hub().Stats().FramesSent, time.Now()
			var press []time.Duration
			for time.Since(start) < window {
				time.Sleep(500 * time.Millisecond)
				t0 := time.Now()
				p.Call(t, "Input.dispatchKeyEvent", map[string]any{"type": "rawKeyDown", "key": "Shift", "code": "ShiftLeft", "windowsVirtualKeyCode": 16})
				press = append(press, time.Since(t0))
				p.Call(t, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": "Shift", "code": "ShiftLeft", "windowsVirtualKeyCode": 16})
			}
			secs := time.Since(start).Seconds()
			after, bytes1, sent1 := metrics(), received.Load(), a.Hub().Stats().FramesSent
			var applied float64
			var long, keys []float64
			p.Eval(t, `window.applied_`, &applied)
			p.Eval(t, `window.long_`, &long)
			p.Eval(t, `window.keys_`, &keys)
			blocking, longest := 0.0, 0.0
			for _, d := range long {
				blocking += d - 50
				longest = max(longest, d)
			}
			slowest := slices.Max(press)
			key := fmt.Sprintf("%d ms", slowest.Milliseconds())
			if len(keys) > 0 {
				key += fmt.Sprintf(" (Event Timing %.0f ms)", slices.Max(keys))
			}
			mu.Lock()
			kind := "fast"
			for _, u := range streams {
				if strings.Contains(u, "Save-Data: on") {
					kind = "slow"
				}
			}
			mu.Unlock()
			t.Logf("| %s | %s | %s | %.1f | %.1f | %.1f | %.0f%% | %d, %.0f ms blocking | %.0f ms | %s |", pg.name, l.name, kind,
				float64(bytes1-bytes0)/secs/1000, float64(sent1-sent0)/secs, applied/secs,
				(after["TaskDuration"]-before["TaskDuration"])/secs*100, len(long), blocking, longest, key)
			if errs := p.Errors(); len(errs) > 0 {
				t.Errorf("%s, %s: %v", pg.name, l.name, errs)
			}
			p.Close(t)
		}
	}
}
