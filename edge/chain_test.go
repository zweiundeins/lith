package edge

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// TestChain runs every piece together, in the order the package doc gives,
// over a real connection.
func TestChain(t *testing.T) {
	log, logs := testLogger()
	static, err := NewStatic(fstest.MapFS{"site.css": {Data: []byte(strings.Repeat(".card { color: red }\n", 50))}}, "/static/")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.Handle("GET /static/", static)
	mux.Handle("GET /healthz", Health("v1"))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<script nonce="%s" type="importmap">{}</script><link href="%s">%s`, Nonce(r.Context()), static.URL("site.css"), page)
	})
	mux.HandleFunc("GET /live", func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Time{}); err != nil {
			t.Errorf("a stream could not clear its deadline: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: datastar-patch-signals\ndata: signals {count: 1}\n\n"))
		rc.Flush()
		<-release
	})
	mux.HandleFunc("POST /cmd/move", func(w http.ResponseWriter, r *http.Request) {
		if SessionID(r.Context()) == "" {
			t.Error("an action without a session id")
		}
		w.WriteHeader(http.StatusNoContent)
	})

	h := http.Handler(mux)
	h = Sessions{Secure: true}.Handler(h)
	h = CSRF{Datastar: []string{"POST /cmd/"}}.Handler(h)
	h = SecurityHeaders{CSP: DefaultCSP(), HSTS: true}.Handler(h)
	h = Compress(h)
	h = Recover(log, h)
	h = LogRequests(log, h)
	srv := httptest.NewServer(h)
	defer srv.Close()

	do := func(method, path string, header ...string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		req.Header.Set("Accept-Encoding", "br")
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// The page: compressed, with the nonce of its own policy and a session cookie.
	res := do("GET", "/")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	html := string(decode(t, res.Header.Get("Content-Encoding"), body))
	m := regexp.MustCompile(`'nonce-([^']+)'`).FindStringSubmatch(res.Header.Get("Content-Security-Policy"))
	if res.Header.Get("Content-Encoding") != "br" || m == nil || !strings.Contains(html, `nonce="`+m[1]+`"`) {
		t.Errorf("page: %v\n%.80s", res.Header, html)
	}
	if !strings.HasPrefix(res.Header.Get("Set-Cookie"), "__Host-sid=") || res.Header.Get("Strict-Transport-Security") == "" {
		t.Errorf("page headers: %v", res.Header)
	}

	// Its stylesheet, by the hashed name the page links.
	css := regexp.MustCompile(`href="([^"]+)"`).FindStringSubmatch(html)[1]
	res = do("GET", css)
	res.Body.Close()
	if res.Header.Get("Content-Encoding") != "br" || res.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("stylesheet %s: %v", css, res.Header)
	}

	// A cross-site action is refused before it gets a session.
	res = do("POST", "/cmd/move", "Sec-Fetch-Site", "cross-site", "Origin", "https://evil.test", "Datastar-Request", "true")
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden || res.Header.Get("Set-Cookie") != "" {
		t.Errorf("cross-site action: %d, Set-Cookie %q", res.StatusCode, res.Header.Get("Set-Cookie"))
	}
	res = do("POST", "/cmd/move", "Sec-Fetch-Site", "same-origin", "Datastar-Request", "true")
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Errorf("same-origin action: %d", res.StatusCode)
	}

	// The stream's first event arrives while the handler is still running.
	res = do("GET", "/live")
	event, err := bufio.NewReader(res.Body).ReadString('\n')
	if err != nil || event != "event: datastar-patch-signals\n" || res.Header.Get("Content-Encoding") != "" {
		t.Errorf("stream: %q %v %v", event, err, res.Header)
	}
	close(release)
	res.Body.Close()

	res = do("GET", "/healthz")
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "ok v1\n" {
		t.Errorf("healthz: %q", body)
	}

	srv.Close() // waits for the stream's handler, and its log line
	if n := strings.Count(logs.String(), "msg=request"); n != 6 {
		t.Errorf("%d request lines for 6 requests:\n%s", n, logs)
	}
}
