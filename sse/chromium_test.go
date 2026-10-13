package sse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
)

// chromiumPage reads three streams with fetch, as Datastar does, and posts
// each event back to /ack as it arrives: the server sends the next one
// only then. It ends by posting what it saw to /result.
const chromiumPage = `<!doctype html><title>frames</title>
<img src="/wait" alt="" hidden>
<script type="module">
const out = {}
for (const name of ["hibernate", "raw", "brotli-one", "brotli"]) {
  const got = []
  try {
    const res = await fetch("/stream/" + name)
    const reader = res.body.pipeThrough(new TextDecoderStream()).getReader()
    let text = ""
    for (;;) {
      const { value, done } = await reader.read()
      if (done) break
      text += value
      let i
      while ((i = text.indexOf("\n\n")) >= 0) {
        const event = text.slice(0, i + 2)
        text = text.slice(i + 2)
        got.push(event.length)
        await fetch("/ack/" + name, { method: "POST", body: event })
      }
    }
    out[name] = { events: got, encoding: res.headers.get("content-encoding"), rest: text.length }
  } catch (e) {
    out[name] = { events: got, error: String(e) }
  }
}
out.userAgent = navigator.userAgent
await fetch("/result", { method: "POST", body: JSON.stringify(out) })
</script>`

func findChromium(t *testing.T) string {
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

// chromiumEvent is the i-th event of a test stream. Every second one
// carries 55 KB of rows that repeat, so that a frame refers back within
// itself.
func chromiumEvent(name string, i int) []byte {
	body := fmt.Sprintf(`<p id="%s-%d">event %d of the %s stream</p>`, name, i, i, name)
	if i%2 == 1 {
		body += strings.Repeat(fmt.Sprintf(`<li class="row">row of %s</li>`, name), 1500)
	}
	return PatchElements([]byte(body))
}

// TestChromiumDecodesFrames checks what a browser makes of bodies that a
// single encoder did not write. A zstd body of several frames, with
// hibernation between events and raw frames while the budget has no room,
// decodes event by event, each as soon as it is sent. A brotli body that
// holds a second brotli stream after the first does not.
//
// Chromium runs with its sandbox, or without one where the machine has
// none and LITH_CHROME_NO_SANDBOX=1 (the page is the test's own).
func TestChromiumDecodesFrames(t *testing.T) {
	if testing.Short() {
		t.Skip("starts Chromium")
	}
	chrome := findChromium(t)
	var mu sync.Mutex
	acks := map[string]chan string{}
	for _, name := range []string{"hibernate", "raw", "brotli-one", "brotli"} {
		acks[name] = make(chan string)
	}
	accepted := map[string]string{}
	result := make(chan []byte, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	fail := func(format string, args ...any) {
		t.Errorf(format, args...)
		releaseOnce.Do(func() { close(release) })
	}

	// await waits for the page to post back the event, and checks it.
	await := func(name string, want []byte) bool {
		select {
		case got := <-acks[name]:
			if got != string(want) {
				fail("%s: the page got %d bytes, want %d: %.80q", name, len(got), len(want), got)
				return false
			}
			return true
		case <-time.After(20 * time.Second):
			fail("%s: no ack for %.60q", name, want)
			return false
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, chromiumPage)
	})
	mux.HandleFunc("GET /wait", func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /ack/{name}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case acks[r.PathValue("name")] <- string(b):
		case <-time.After(20 * time.Second):
		}
	})
	mux.HandleFunc("POST /result", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		result <- b
		releaseOnce.Do(func() { close(release) })
	})
	mux.HandleFunc("GET /stream/hibernate", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		accepted["hibernate"] = r.Header.Get("Accept-Encoding")
		mu.Unlock()
		s, err := Open(w, r, Options{})
		if err != nil || s.Encoding() != Zstd {
			fail("the browser's stream: %v, coding %q", err, s.Encoding())
			return
		}
		defer s.Close()
		// Two events per frame, the second referring to the first; a
		// hibernation between the frames.
		for i := range 6 {
			e := chromiumEvent("hibernate", i)
			if err := s.Write(e); err != nil || !await("hibernate", e) {
				fail("hibernate: event %d: %v", i, err)
				return
			}
			if i%2 == 1 {
				if err := s.Hibernate(); err != nil {
					fail("hibernate: %v", err)
				}
			}
		}
		last := chromiumEvent("hibernate", 6)
		go await("hibernate", last)
		s.CloseWith(last)
	})
	mux.HandleFunc("GET /stream/raw", func(w http.ResponseWriter, r *http.Request) {
		b := NewBudget(BudgetOptions{Limit: 1 << 20, MinWindow: 16, MaxWindow: 16})
		hog, _ := Open(newRecorder(), request(r.Context(), Zstd), Options{Budget: b})
		s, err := Open(w, r, Options{Budget: b})
		if err != nil || !s.asleep {
			fail("raw: %v, asleep %v", err, s.asleep)
			return
		}
		defer s.Close()
		for i := range 6 {
			switch i {
			case 2: // room frees up: the stream compresses
				hog.Close()
			case 4: // it hibernates, and the room is taken again
				s.Hibernate()
				hog, _ = Open(newRecorder(), request(r.Context(), Zstd), Options{Budget: b})
			}
			e := chromiumEvent("raw", i)
			if err := s.Write(e); err != nil || !await("raw", e) {
				fail("raw: event %d: %v", i, err)
				return
			}
			if compressed := !s.asleep; compressed != (i == 2 || i == 3) {
				fail("raw: event %d went out compressed: %v", i, compressed)
			}
		}
		hog.Close()
	})
	// One brotli stream with two events: brotli itself decodes.
	mux.HandleFunc("GET /stream/brotli-one", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		accepted["brotli"] = r.Header.Get("Accept-Encoding")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "br")
		bw := brotli.NewWriterLevel(w, 4)
		for i := range 2 {
			bw.Write(chromiumEvent("brotli-one", i))
			bw.Flush()
			w.(http.Flusher).Flush()
			if !await("brotli-one", chromiumEvent("brotli-one", i)) {
				return
			}
		}
		bw.Close()
	})
	// Two brotli streams, one after the other, as a hibernating brotli
	// stream would send them.
	mux.HandleFunc("GET /stream/brotli", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "br")
		for i := range 2 {
			var buf bytes.Buffer
			bw := brotli.NewWriterLevel(&buf, 4)
			bw.Write(chromiumEvent("brotli", i))
			bw.Close()
			w.Write(buf.Bytes())
			w.(http.Flusher).Flush()
			if i == 0 && !await("brotli", chromiumEvent("brotli", i)) {
				return
			}
		}
		// The page acks a second event only if it decodes one.
		select {
		case got := <-acks["brotli"]:
			mu.Lock()
			accepted["brotli second"] = got
			mu.Unlock()
		case <-time.After(3 * time.Second):
		}
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	args := []string{"--headless=new", "--disable-gpu", "--no-first-run", "--user-data-dir=" + t.TempDir()}
	if os.Getenv("LITH_CHROME_NO_SANDBOX") == "1" {
		args = append(args, "--no-sandbox")
	}
	cmd := exec.Command(chrome, append(args, "--dump-dom", "http://"+ln.Addr().String()+"/")...)
	var stderr strings.Builder
	cmd.Stdout, cmd.Stderr = io.Discard, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if strings.Contains(stderr.String(), "No usable sandbox") {
			t.Skip("Chromium has no usable sandbox here; set LITH_CHROME_NO_SANDBOX=1")
		}
		if err != nil {
			t.Fatalf("chromium: %v\n%s", err, stderr.String())
		}
	case <-time.After(90 * time.Second):
		cmd.Process.Kill()
		t.Fatal("chromium timed out")
	}
	var out map[string]json.RawMessage
	select {
	case b := <-result:
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("the page posted no result\n%s", stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	t.Logf("%s, Accept-Encoding %q (brotli's %q)", out["userAgent"], accepted["hibernate"], accepted["brotli"])
	for name, n := range map[string]int{"hibernate": 7, "raw": 6, "brotli-one": 2} {
		var r struct {
			Events   []int
			Encoding string
			Error    string
			Rest     int
		}
		json.Unmarshal(out[name], &r)
		if r.Error != "" || len(r.Events) != n || r.Rest != 0 {
			t.Errorf("%s: %s", name, out[name])
		}
		t.Logf("%s: %s", name, out[name])
	}
	t.Logf("brotli, two streams in one body: %s; a second event decoded: %v", out["brotli"], accepted["brotli second"] != "")
}
