package edge

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestPrecompress(t *testing.T) {
	body := []byte(strings.Repeat("rocket('sb-x', { render: () => html`<p>hi</p>` })\n", 100))
	br, gz := Precompress(body)
	if !bytes.Equal(decode(t, "br", br), body) || !bytes.Equal(decode(t, "gzip", gz), body) {
		t.Fatal("the precompressed forms do not decode to the body")
	}
	br2, _ := Precompress(bytes.Clone(body))
	if &br[0] != &br2[0] {
		t.Error("the same content was compressed twice")
	}
}

func TestServePrecompressed(t *testing.T) {
	body := []byte(strings.Repeat("rocket('sb-x', { render: () => html`<p>hi</p>` })\n", 100))
	get := func(accept, etag string, b []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/c/x.js", nil)
		r.Header.Set("Accept-Encoding", accept)
		if etag != "" {
			r.Header.Set("If-None-Match", etag)
		}
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		ServePrecompressed(w, r, b)
		return w
	}
	tags := map[string]bool{}
	for accept, coding := range map[string]string{"gzip, deflate, br, zstd": "br", "gzip": "gzip", "zstd": "", "": "", "br;q=0, gzip": "gzip"} {
		w := get(accept, "", body)
		if w.Code != 200 || w.Header().Get("Content-Encoding") != coding || !bytes.Equal(decode(t, coding, w.Body.Bytes()), body) {
			t.Errorf("Accept-Encoding %q: %d %q", accept, w.Code, w.Header().Get("Content-Encoding"))
		}
		if w.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("Accept-Encoding %q: Vary %q", accept, w.Header().Get("Vary"))
		}
		etag := w.Header().Get("ETag")
		tags[etag] = true
		if again := get(accept, etag, body); again.Code != http.StatusNotModified {
			t.Errorf("Accept-Encoding %q: If-None-Match %s gave %d", accept, etag, again.Code)
		}
	}
	if len(tags) != 3 {
		t.Errorf("ETags %v: want one per coding", tags)
	}
	if w := get("br", "", []byte("tiny")); w.Header().Get("Content-Encoding") != "" || w.Body.String() != "tiny" {
		t.Error("a tiny body should go out as it is")
	}
}

var png = []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00\x01binary", 100))

func newTestStatic(t *testing.T) *Static {
	t.Helper()
	s, err := NewStatic(fstest.MapFS{
		"css/site.css":  {Data: []byte(strings.Repeat(".card { color: var(--sb-fg); }\n", 50))},
		"js/app.min.js": {Data: page},
		"img/logo.png":  {Data: png},
		"robots.txt":    {Data: []byte("User-agent: *\n")},
		"LICENSE":       {Data: []byte("MIT")},
	}, "/static/")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStaticURL(t *testing.T) {
	s := newTestStatic(t)
	for name, pattern := range map[string]string{
		"css/site.css":  "/static/css/site.*.css",
		"js/app.min.js": "/static/js/app.min.*.js",
		"LICENSE":       "/static/LICENSE.*",
		"missing.css":   "/static/missing.css",
	} {
		got := s.URL(name)
		prefix, suffix, _ := strings.Cut(pattern, "*")
		hash := strings.TrimSuffix(strings.TrimPrefix(got, prefix), suffix)
		if !strings.HasPrefix(got, prefix) || !strings.HasSuffix(got, suffix) || (name != "missing.css" && !isHash(hash)) {
			t.Errorf("URL(%q) = %q, want %s", name, got, pattern)
		}
		if plain, ok := unhash(strings.TrimPrefix(got, "/static/")); name != "missing.css" && (!ok || plain != name) {
			t.Errorf("unhash(%q) = %q, %v", got, plain, ok)
		}
	}
}

func TestStaticPrefixWithoutSlash(t *testing.T) {
	s, err := NewStatic(fstest.MapFS{"site.css": {Data: []byte(".card {}")}}, "/assets")
	if err != nil {
		t.Fatal(err)
	}
	u := s.URL("site.css")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", u, nil))
	if !strings.HasPrefix(u, "/assets/site.") || w.Code != 200 {
		t.Errorf("URL %q answered %d", u, w.Code)
	}
}

func TestStaticServe(t *testing.T) {
	s := newTestStatic(t)
	get := func(method, path string, header ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		for i := 0; i+1 < len(header); i += 2 {
			r.Header.Set(header[i], header[i+1])
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	css := s.URL("css/site.css")

	w := get("GET", css, "Accept-Encoding", "gzip, br")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" ||
		w.Header().Get("Content-Encoding") != "br" || w.Header().Get("Content-Type") != "text/css; charset=utf-8" ||
		!bytes.HasSuffix([]byte(w.Header().Get("ETag")), []byte(`-br"`)) {
		t.Errorf("hashed css: %d %v", w.Code, w.Header())
	}
	if got := decode(t, "br", w.Body.Bytes()); !strings.HasPrefix(string(got), ".card") {
		t.Error("hashed css: the body did not survive")
	}

	w = get("GET", "/static/css/site.css")
	etag := w.Header().Get("ETag")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" || etag == "" || w.Header().Get("Content-Encoding") != "" {
		t.Errorf("plain css: %d %v", w.Code, w.Header())
	}
	if w = get("GET", "/static/css/site.css", "If-None-Match", etag); w.Code != http.StatusNotModified {
		t.Errorf("revalidating plain css: %d", w.Code)
	}

	w = get("GET", "/static/css/site.000000000000.css")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" || !strings.HasPrefix(w.Body.String(), ".card") {
		t.Errorf("an old hash: %d %v", w.Code, w.Header())
	}

	w = get("GET", s.URL("img/logo.png"), "Accept-Encoding", "br", "Range", "bytes=0-3")
	if w.Code != http.StatusPartialContent || w.Body.String() != "\x89PNG" || w.Header().Get("Content-Type") != "image/png" ||
		w.Header().Get("Content-Encoding") != "" || w.Header().Get("ETag") == "" {
		t.Errorf("png range: %d %v %q", w.Code, w.Header(), w.Body)
	}

	w = get("GET", s.URL("robots.txt"), "Accept-Encoding", "br")
	if w.Code != 200 || w.Body.String() != "User-agent: *\n" || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Errorf("small text: %d %v", w.Code, w.Header())
	}

	w = get("HEAD", css, "Accept-Encoding", "br")
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" || w.Header().Get("Content-Encoding") != "br" {
		t.Errorf("HEAD: %d %v", w.Code, w.Header())
	}

	for path, code := range map[string]int{"/static/nope.css": 404, "/static/": 404, "/other/css/site.css": 404} {
		if w := get("GET", path); w.Code != code {
			t.Errorf("GET %s: %d, want %d", path, w.Code, code)
		}
	}
	if w := get("POST", css); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST: %d %v", w.Code, w.Header())
	}
}

func TestStaticBehindCompress(t *testing.T) {
	s := newTestStatic(t)
	r := httptest.NewRequest("GET", s.URL("js/app.min.js"), nil)
	r.Header.Set("Accept-Encoding", "zstd, br")
	w := httptest.NewRecorder()
	Compress(s).ServeHTTP(w, r)
	if w.Header().Get("Content-Encoding") != "br" || !bytes.Equal(decode(t, "br", w.Body.Bytes()), page) {
		t.Errorf("Compress touched a precompressed file: %v", w.Header())
	}
	if vary := w.Header().Values("Vary"); len(vary) != 1 {
		t.Errorf("Vary %q", vary)
	}
}
