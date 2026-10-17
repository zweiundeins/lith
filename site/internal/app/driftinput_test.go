package app_test

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/browser"
)

// inputLog runs before the page's scripts when TestLifeDrift measures
// input (LIFE_DRIFT_INPUT). A capture listener on window notes when each
// Shift key press reaches the page, and an observer of Chromium's Event
// Timing reports every keydown and keyup of 16 ms or more, both through
// the console, as appliedLog does.
const inputLog = `(() => {
  addEventListener('keydown', e => {
    if (e.key === 'Shift') console.debug('lith-key', performance.timeOrigin + performance.now())
  }, true)
  new PerformanceObserver(l => {
    for (const e of l.getEntries()) {
      if (e.name === 'keydown' || e.name === 'keyup') console.debug('lith-event', e.name, e.duration, e.processingStart - e.startTime)
    }
  }).observe({type: 'event', durationThreshold: 16, buffered: true})
})()`

// inputMeter presses Shift every 500 ms, as TestSlowLinkCost does, and
// keeps how long each press took to reach the page's handler, how long
// DevTools took to return from sending it, and the Event Timing entries.
type inputMeter struct {
	mu      sync.Mutex
	sent    []time.Time
	acked   []time.Duration // Input.dispatchKeyEvent's round trip, by press
	handled []time.Time     // when the page's handler ran, in order
	keydown []float64       // Event Timing durations of keydown, ms
	delay   []float64       // their input delay, ms
	keyup   []float64
}

func (m *inputMeter) follow(p *browser.Page) {
	p.OnEvent("Runtime.consoleAPICalled", func(params json.RawMessage) {
		var e struct {
			Args []struct{ Value any }
		}
		json.Unmarshal(params, &e)
		if len(e.Args) == 0 {
			return
		}
		switch e.Args[0].Value {
		case "lith-key":
			if len(e.Args) == 2 {
				ms, _ := e.Args[1].Value.(float64)
				m.mu.Lock()
				m.handled = append(m.handled, time.UnixMicro(int64(ms*1000)))
				m.mu.Unlock()
			}
		case "lith-event":
			if len(e.Args) == 4 {
				d, _ := e.Args[2].Value.(float64)
				w, _ := e.Args[3].Value.(float64)
				m.mu.Lock()
				if e.Args[1].Value == "keydown" {
					m.keydown, m.delay = append(m.keydown, d), append(m.delay, w)
				} else {
					m.keyup = append(m.keyup, d)
				}
				m.mu.Unlock()
			}
		}
	})
}

// press sends Shift down and up every 500 ms until stop closes. Each press
// goes out on its own goroutine, so a page that holds one up for seconds
// does not delay the next.
func (m *inputMeter) press(p *browser.Page, stop <-chan struct{}) {
	tk := time.NewTicker(500 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tk.C:
		}
		m.mu.Lock()
		i := len(m.sent)
		t0 := time.Now()
		m.sent = append(m.sent, t0)
		m.acked = append(m.acked, -1)
		m.mu.Unlock()
		go func() {
			p.TryCall("Input.dispatchKeyEvent", map[string]any{"type": "rawKeyDown", "key": "Shift", "code": "ShiftLeft", "windowsVirtualKeyCode": 16})
			d := time.Since(t0)
			m.mu.Lock()
			m.acked[i] = d
			m.mu.Unlock()
			p.TryCall("Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": "Shift", "code": "ShiftLeft", "windowsVirtualKeyCode": 16})
		}()
	}
}

// summary logs the percentiles and writes every press to path, if set.
func (m *inputMeter) summary(t *testing.T, start time.Time, path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var handler, acked []float64
	pending := 0
	for i, s := range m.sent {
		if i < len(m.handled) {
			handler = append(handler, float64(m.handled[i].Sub(s).Microseconds())/1000)
		} else {
			pending++
		}
		if m.acked[i] >= 0 {
			acked = append(acked, float64(m.acked[i].Microseconds())/1000)
		}
	}
	// Event Timing reports only entries of 16 ms or more: the presses
	// without one count as 0 for the percentile.
	n := len(m.handled)
	events := slices.Clone(m.keydown)
	for len(events) < n {
		events = append(events, 0)
	}
	t.Logf("input: %d presses, %d handled, %d not by the end; to the handler median %.0f ms, p95 %.0f ms, max %.0f ms; DevTools' dispatch median %.0f ms, p95 %.0f ms, max %.0f ms; Event Timing keydown %d of 16 ms or more, p98 %.0f ms, max %.0f ms, input delay max %.0f ms; keyup max %.0f ms",
		len(m.sent), n, pending, pct(handler, 50), pct(handler, 95), pct(handler, 100), pct(acked, 50), pct(acked, 95), pct(acked, 100),
		len(m.keydown), pct(events, 98), pct(events, 100), pct(m.delay, 100), pct(m.keyup, 100))
	if path == "" {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		t.Error(err)
		return
	}
	defer f.Close()
	fmt.Fprintln(f, "t,handler_ms,dispatch_ms")
	for i, s := range m.sent {
		h, a := "", ""
		if i < len(m.handled) {
			h = fmt.Sprintf("%.1f", float64(m.handled[i].Sub(s).Microseconds())/1000)
		}
		if m.acked[i] >= 0 {
			a = fmt.Sprintf("%.1f", float64(m.acked[i].Microseconds())/1000)
		}
		fmt.Fprintf(f, "%.3f,%s,%s\n", s.Sub(start).Seconds(), h, a)
	}
}

// pct is the pth percentile of xs by the nearest rank, 0 for none.
func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Sorted(slices.Values(xs))
	i := int(float64(len(s))*p/100+0.999999) - 1
	return s[min(max(i, 0), len(s)-1)]
}
