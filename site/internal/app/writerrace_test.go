package app_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/app"
	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
	writerrace "github.com/zweiundeins/lith/site/internal/demos/writer-race"
)

// writerRaceStart presses Start on a writer race page with the producers and
// the durability given.
func writerRaceStart(t *testing.T, p *browser.Page, producers int, sync string) {
	t.Helper()
	p.Eval(t, fmt.Sprintf(`(() => {
		document.querySelector('input[name="writer-race-producers"][value="%d"]').click()
		document.querySelector('input[name="writer-race-sync"][value="%s"]').click()
		document.querySelector('.wr-controls button[type=submit]').click()
	})()`, producers, sync), nil)
}

// writerRaceOver waits until no race waits or runs.
func writerRaceOver(t *testing.T, a *app.App) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var n int
		if err := a.DB().R.QueryRow(`SELECT count(*) FROM writer_race_races WHERE state IN ('queued', 'running')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d races still in line or on the track", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestWriterRaceStream runs races over HTTP: one tab starts a race and
// its stream shows it join the line, run tick by tick and end; another
// session's race waits in line meanwhile and runs after it. When both are
// over, no race file is left beside the database.
func TestWriterRaceStream(t *testing.T) {
	a, base := apptest.Start(t)
	ada, bob := client(t), client(t)
	get(t, ada, base+"/demos/writer-race") // the sessions' cookies
	get(t, bob, base+"/demos/writer-race")
	events := listen(t, ada, base+"/demos/writer-race/stream?tab=tab1")
	next(t, events, `id="writer-race-track"`)
	next(t, events, `"_writerRace":{"ack":"","mine":0,"active":false}`)

	if code := post(t, ada, base+"/cmd/writer-race/start", `{"tab":"tab1","op":"op1","producers":50,"sync":"NORMAL"}`); code != http.StatusNoContent {
		t.Fatalf("start: %d", code)
	}
	// The line and the tab's own view change in the same tick, and reach
	// the stream in the order it watches them.
	next(t, events, `id="writer-race-wait-1"`)
	next(t, events, `"ack":"op1","mine":1,"active":true`)
	if code := post(t, bob, base+"/cmd/writer-race/start", `{"tab":"tab2","op":"op1","producers":1,"sync":"FULL"}`); code != http.StatusNoContent {
		t.Fatalf("bob's start: %d", code)
	}
	running := next(t, events, `data-state="running"`)
	if !strings.Contains(running, "50 producers") || !strings.Contains(running, `<sb-meter label="Queue"`) {
		t.Errorf("the track of a running race:\n%s", running)
	}
	// Samples come once a tick: several frames of the running track.
	for range 3 {
		next(t, events, `id="writer-race-track" class="wr-track" data-state="running"`)
	}
	next(t, events, `id="writer-race-result-1"`)
	next(t, events, `"ack":"op1","mine":1,"active":false`)

	// The same session again while bob's race waits or runs: refused.
	if code := post(t, bob, base+"/cmd/writer-race/start", `{"tab":"tab3","op":"op2","producers":1,"sync":"FULL"}`); code != http.StatusNoContent {
		t.Fatalf("bob's second start: %d", code)
	}
	bobs := listen(t, bob, base+"/demos/writer-race/stream?tab=tab3")
	next(t, bobs, "Race 2 is yours and")
	next(t, events, `id="writer-race-result-2"`)
	writerRaceOver(t, a)

	// Bad requests never reach the writer, each from a session of its own
	// (a session starts two races at once, then one in ten seconds).
	for _, body := range []string{`{"tab":"tab1","op":"op3","producers":100,"sync":"NORMAL"}`, `{"tab":"tab1","op":"op3","producers":1,"sync":"OFF"}`, `{"tab":"","op":"op3","producers":1,"sync":"FULL"}`} {
		if code := post(t, client(t), base+"/cmd/writer-race/start", body); code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, code)
		}
	}
	for range 2 {
		post(t, ada, base+"/cmd/writer-race/start", `{"tab":"tab1","op":"op4","producers":1,"sync":"OFF"}`)
	}
	if code := post(t, ada, base+"/cmd/writer-race/start", `{"tab":"tab1","op":"op5","producers":1,"sync":"FULL"}`); code != http.StatusTooManyRequests {
		t.Errorf("a session's fourth start in a few seconds: %d", code)
	}
	if files, _ := filepath.Glob(filepath.Join(filepath.Dir(a.DB().Path), "writer-race-*")); len(files) != 0 {
		t.Errorf("race files left behind: %v", files)
	}
}

// TestWriterRace starts a race in one browser and watches it in another:
// the second sees it run, tick by tick, and end with its result, and only
// the first hears that the race is its own.
//
//	LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run 'TestWriterRace$'
func TestWriterRace(t *testing.T) {
	a, base := apptest.Start(t)
	b := browser.Start(t)
	ada := b.NewPage(t, 1280, 900)
	ada.Navigate(t, base+"/demos/writer-race")
	bob := b.NewPage(t, 1280, 900)
	bob.Navigate(t, base+"/demos/writer-race")
	for _, p := range []*browser.Page{ada, bob} {
		p.Wait(t, `document.querySelector('.live-badge.is-live') && customElements.get('sb-gauge') && customElements.get('sb-meter')`, 10*time.Second)
	}
	waitFor(t, "two tabs on the track", func() bool { return a.Hub().Count("writer-race:track") == 2 })

	writerRaceStart(t, ada, 200, "FULL")
	ada.Wait(t, `document.querySelector('#writer-race-outcome').textContent.includes('Race 1 is yours')`, 10*time.Second)
	ada.Wait(t, `document.querySelector('.wr-controls button[type=submit]').disabled`, 5*time.Second)
	bob.Wait(t, `document.querySelector('#writer-race-wait-1') || document.querySelector('#writer-race-track[data-state="running"]')`, 10*time.Second)
	bob.Wait(t, `document.querySelector('#writer-race-track[data-state="running"]')`, 10*time.Second)
	// The track changes from tick to tick while the race runs.
	var first, later string
	bob.Eval(t, `document.querySelector('.wr-chart__line')?.getAttribute('d') ?? ''`, &first)
	time.Sleep(300 * time.Millisecond)
	bob.Eval(t, `document.querySelector('.wr-chart__line').getAttribute('d')`, &later)
	if first == later {
		t.Errorf("the chart did not move in 300 ms of the race: %q", later)
	}
	bob.Wait(t, `Number(document.querySelector('.wr-tile--queue sb-meter').getAttribute('max')) === 200`, time.Second)
	bob.Wait(t, `document.querySelector('#writer-race-track[data-state="done"] .wr-result')`, 15*time.Second)
	bob.Wait(t, `document.querySelector('#writer-race-result-1 .wr-table__rate')`, 5*time.Second)
	var outcome string
	bob.Eval(t, `document.querySelector('#writer-race-outcome').textContent.trim()`, &outcome)
	if outcome != "" {
		t.Errorf("the other tab hears %q about a race it did not start", outcome)
	}
	var mine, theirs bool
	ada.Wait(t, `document.querySelector('#writer-race-result-1.is-mine')`, 5*time.Second)
	bob.Eval(t, `document.querySelector('#writer-race-result-1').classList.contains('is-mine')`, &theirs)
	ada.Eval(t, `!document.querySelector('.wr-controls button[type=submit]').disabled`, &mine)
	if theirs || !mine {
		t.Errorf("the race is marked as bob's: %v; ada may start again: %v", theirs, mine)
	}
	writerRaceOver(t, a)
	for name, p := range map[string]*browser.Page{"ada": ada, "bob": bob} {
		if errs := p.Errors(); len(errs) > 0 {
			t.Errorf("%s: %v", name, errs)
		}
	}
}

// TestShotsWriterRace writes screenshots of the writer race: during a
// race and after it, in both themes and the smooth style, on a phone, and
// the landing page's slot.
//
//	LITH_SITE_SHOTS=/tmp/shots LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestShotsWriterRace
func TestShotsWriterRace(t *testing.T) {
	dir := os.Getenv("LITH_SITE_SHOTS")
	if dir == "" {
		t.Skip("set LITH_SITE_SHOTS to a directory for the screenshots")
	}
	a, base := apptest.Start(t)
	b := browser.Start(t)
	p := b.NewPage(t, 1440, 1000)
	p.Navigate(t, base+"/demos/writer-race")
	p.Eval(t, "document.fonts.ready.then(() => true)", nil)
	p.Wait(t, `document.querySelector('.live-badge.is-live')`, 10*time.Second)
	time.Sleep(500 * time.Millisecond)
	p.Screenshot(t, filepath.Join(dir, "writer-race-empty.png"), true)

	writerRaceStart(t, p, 200, "NORMAL")
	p.Wait(t, `document.querySelector('#writer-race-track[data-state="running"]')`, 10*time.Second)
	time.Sleep(1500 * time.Millisecond)
	p.Screenshot(t, filepath.Join(dir, "writer-race-running.png"), true)
	p.ShotElement(t, "#writer-race-track", filepath.Join(dir, "detail-writer-race-running.png"), 1.5)
	p.Wait(t, `document.querySelector('#writer-race-track[data-state="done"]')`, 15*time.Second)
	writerRaceOver(t, a)
	time.Sleep(1200 * time.Millisecond)
	p.Screenshot(t, filepath.Join(dir, "writer-race-done.png"), true)

	shot := func(name, path string, w, h int, setup string) {
		q := b.NewPage(t, w, h)
		defer q.Close(t)
		if setup != "" {
			q.Navigate(t, base+"/")
			q.Eval(t, setup, nil)
		}
		q.Navigate(t, base+path)
		q.Eval(t, "document.fonts.ready.then(() => true)", nil)
		time.Sleep(900 * time.Millisecond)
		q.Screenshot(t, filepath.Join(dir, name+".png"), true)
		if errs := q.Errors(); len(errs) > 0 {
			t.Errorf("%s: %v", name, errs)
		}
	}
	shot("writer-race-light", "/demos/writer-race", 1440, 1000, "document.cookie = 'lith-theme=light; Path=/'")
	shot("writer-race-smooth", "/demos/writer-race", 1440, 1000, "document.cookie = 'lith-style=smooth; Path=/'")
	shot("mobile-writer-race", "/demos/writer-race", 390, 844, "")

	home := b.NewPage(t, 1440, 900)
	home.Call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "reduce"}}})
	home.Navigate(t, base+"/")
	home.Eval(t, "document.fonts.ready.then(() => true)", nil)
	home.Wait(t, `document.querySelector('[data-demo="writer-race"] .wr-embed__head.is-live')`, 10*time.Second)
	home.ShotElement(t, "#linear", filepath.Join(dir, "detail-writer-race-slot.png"), 1.5)
	home.Eval(t, `document.querySelector('.wr-embed__go button').click()`, nil)
	home.Wait(t, `document.querySelector('#writer-race-embed[data-state="running"]')`, 10*time.Second)
	time.Sleep(1500 * time.Millisecond)
	home.ShotElement(t, "#linear", filepath.Join(dir, "detail-writer-race-slot-running.png"), 1.5)
	writerRaceOver(t, a)
	for name, q := range map[string]*browser.Page{"the race": p, "the landing page": home} {
		if errs := q.Errors(); len(errs) > 0 {
			t.Errorf("%s: %v", name, errs)
		}
	}
}

// TestWriterRaceCost measures what races cost the site: the process's CPU
// and memory while races of every setting run, and the bytes and frames
// their tabs get, with streams open on the race's page:
//
//	LITH_WRITER_RACE_COST=100 go test ./internal/app -run TestWriterRaceCost -v
//
// The number is how many streams watch; GOMAXPROCS=2 plays a smaller host.
func TestWriterRaceCost(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("LITH_WRITER_RACE_COST"))
	if n <= 0 {
		t.Skip("set LITH_WRITER_RACE_COST to the number of streams")
	}
	a, base := apptest.Start(t)
	var wire atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := range n {
		c := client(t)
		get(t, c, base+"/demos/writer-race")
		req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/demos/writer-race/stream?tab=cost%d", base, i), nil)
		req.Header.Set("Accept-Encoding", "zstd")
		res, err := c.Do(req)
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("stream %d: %v %v", i, err, res)
		}
		go func() {
			defer res.Body.Close()
			buf := make([]byte, 32<<10)
			for {
				k, err := res.Body.Read(buf)
				wire.Add(int64(k))
				if err != nil {
					return
				}
			}
		}()
	}
	waitFor(t, "the streams", func() bool { return a.Hub().Count("writer-race:track") == n })
	time.Sleep(time.Second)

	type peak struct{ rss, heap uint64 }
	measure := func(d time.Duration, during func()) (cores float64, p peak) {
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			tk := time.NewTicker(20 * time.Millisecond)
			defer tk.Stop()
			for {
				var ms goruntime.MemStats
				goruntime.ReadMemStats(&ms)
				p.heap = max(p.heap, ms.HeapInuse)
				p.rss = max(p.rss, writerRaceRSS())
				select {
				case <-stop:
					return
				case <-tk.C:
				}
			}
		}()
		c0, w0 := writerRaceCPU(), time.Now()
		if during != nil {
			during()
		} else {
			time.Sleep(d)
		}
		cores = (writerRaceCPU() - c0).Seconds() / time.Since(w0).Seconds()
		close(stop)
		<-done
		return cores, p
	}
	idle, p := measure(3*time.Second, nil)
	t.Logf("GOMAXPROCS %d, %d streams on the race's page, idle: %.2f cores, RSS %d MiB, heap %d MiB", goruntime.GOMAXPROCS(0), n, idle, p.rss>>20, p.heap>>20)
	for i, s := range []struct {
		producers int
		sync      string
	}{{200, "NORMAL"}, {200, "FULL"}, {50, "NORMAL"}, {1, "FULL"}} {
		c := client(t)
		get(t, c, base+"/demos/writer-race")
		h0, b0 := a.Hub().Stats(), wire.Load()
		var started, ended time.Time
		cores, p := measure(0, func() {
			body := fmt.Sprintf(`{"tab":"start%d","op":"op","producers":%d,"sync":%q}`, i, s.producers, s.sync)
			if code := post(t, c, base+"/cmd/writer-race/start", body); code != http.StatusNoContent {
				t.Fatalf("start: %d", code)
			}
			started = time.Now()
			writerRaceOver(t, a)
			ended = time.Now()
		})
		h := a.Hub().Stats()
		var rate float64
		a.DB().R.QueryRow(`SELECT CAST(commands AS REAL) * 1000 / elapsed_ms FROM writer_race_races ORDER BY id DESC LIMIT 1`).Scan(&rate)
		frames := h.FramesSent - h0.FramesSent
		t.Logf("%3d %-6s: %6.0f commands/s; site %.2f cores over %.1f s, peak RSS %d MiB, heap %d MiB; %d ticks, %d frames (%d dropped, %d overruns), %.0f bytes a frame on the wire",
			s.producers, s.sync, rate, cores, ended.Sub(started).Seconds(), p.rss>>20, p.heap>>20,
			h.Ticks-h0.Ticks, frames, h.FramesDropped-h0.FramesDropped, h.Overruns-h0.Overruns, float64(wire.Load()-b0)/float64(max(frames, 1)))
		time.Sleep(writerrace.Pause)
	}
}

func writerRaceCPU() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// writerRaceRSS is the process's resident memory, from /proc.
func writerRaceRSS() uint64 {
	b, _ := os.ReadFile("/proc/self/statm")
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	pages, _ := strconv.ParseUint(f[1], 10, 64)
	return pages * uint64(os.Getpagesize())
}
