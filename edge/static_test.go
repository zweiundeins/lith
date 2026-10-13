package edge

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/maphash"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestPrecompressGzipIsTheStandardLibrarys(t *testing.T) {
	for _, body := range [][]byte{page, bytes.Repeat(page, 20), []byte("tiny")} {
		_, gz := Precompress(body)
		var want bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&want, gzip.BestCompression)
		zw.Write(body)
		zw.Close()
		if !bytes.Equal(gz, want.Bytes()) {
			t.Errorf("%d bytes: gzip -9 of %d bytes, compress/gzip's of %d", len(body), len(gz), want.Len())
		}
	}
}

func TestBrotliLen(t *testing.T) {
	for _, body := range [][]byte{nil, []byte("tiny"), page, bytes.Repeat(page, 20)} {
		body = append(bytes.Clone(body), "measured, not served"...)
		n := BrotliLen(body)
		if _, kept := precompressCache.Load(sha256.Sum256(body)); kept {
			t.Errorf("%d bytes: BrotliLen kept its compression", len(body))
		}
		if br, _ := Precompress(body); n != len(br) {
			t.Errorf("%d bytes: BrotliLen = %d, Precompress gave %d", len(body), n, len(br))
		}
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

func TestServePrecompressedHashesOnce(t *testing.T) {
	serve := func(b []byte, ifNoneMatch string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/about", nil)
		r.Header.Set("Accept-Encoding", "br")
		if ifNoneMatch != "" {
			r.Header.Set("If-None-Match", ifNoneMatch)
		}
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		ServePrecompressed(w, r, b)
		return w
	}
	body := []byte(strings.Repeat("<p>a page rendered at startup</p>\n", 40))
	etag := serve(body, "").Header().Get("ETag")
	first, ok := bodySums.Load(maphash.Bytes(bodySeed, body))
	if !ok {
		t.Fatal("the body's hash was not kept")
	}
	if w := serve(bytes.Clone(body), ""); w.Header().Get("ETag") != etag {
		t.Errorf("the same content in another slice: ETag %s, want %s", w.Header().Get("ETag"), etag)
	}
	if again, _ := bodySums.Load(maphash.Bytes(bodySeed, body)); again != first {
		t.Error("the same content was hashed again")
	}
	for _, inm := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
		if w := serve(body, inm); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
			t.Errorf("If-None-Match %s: %d", inm, w.Code)
		}
	}
	if w := serve(body, `"other"`); w.Code != http.StatusOK {
		t.Errorf("If-None-Match of another content: %d", w.Code)
	}

	// A buffer rendered into again holds another content: its hash is not
	// taken for the new bytes, nor the new bytes for the old content's.
	buf := []byte(strings.Repeat("<p>render one</p>\n", 40))
	old := bytes.Clone(buf)
	oldTag := serve(buf, "").Header().Get("ETag")
	copy(buf, strings.Repeat("<p>render two</p>\n", 40))
	newer := bytes.Clone(buf)
	for _, c := range []struct {
		b    []byte
		want []byte
	}{{buf, newer}, {old, old}} {
		sum := sha256.Sum256(c.want)
		w := serve(c.b, "")
		if tag := w.Header().Get("ETag"); tag != `"`+etagOf(sum)+`-br"` || !bytes.Equal(decode(t, "br", w.Body.Bytes()), c.want) {
			t.Errorf("%.20q: ETag %s, the old one %s", c.want, tag, oldTag)
		}
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
		if !strings.HasPrefix(got, prefix) || !strings.HasSuffix(got, suffix) || (name != "missing.css" && (len(hash) != 12 || !isHash(hash))) {
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

func serveStatic(s *Static, path string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestStaticAdd(t *testing.T) {
	s := newTestStatic(t)
	immutable := "public, max-age=31536000, immutable"

	card := append(bytes.Clone(png), "card"...)
	s.Add("og/home.png", card)
	w := serveStatic(s, s.URL("og/home.png"), "Range", "bytes=0-3")
	if w.Code != http.StatusPartialContent || w.Body.String() != "\x89PNG" || w.Header().Get("Content-Type") != "image/png" ||
		w.Header().Get("Cache-Control") != immutable || w.Header().Get("ETag") == "" {
		t.Errorf("added png: %d %v %q", w.Code, w.Header(), w.Body)
	}

	css := []byte(strings.Repeat(".og { color: red }\n", 50))
	s.Add("css/og.css", css)
	w = serveStatic(s, s.URL("css/og.css"), "Accept-Encoding", "br")
	if w.Code != 200 || w.Header().Get("Content-Encoding") != "br" || !bytes.Equal(decode(t, "br", w.Body.Bytes()), css) {
		t.Errorf("added css: %d %v", w.Code, w.Header())
	}

	// Replacing a file: its old URL gets the new content, uncached.
	old := s.URL("css/site.css")
	newer := []byte(strings.Repeat(".card { color: blue; }\n", 50))
	s.Add("css/site.css", newer)
	if s.URL("css/site.css") == old {
		t.Fatal("the URL did not change with the content")
	}
	w = serveStatic(s, old)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" || w.Body.String() != string(newer) {
		t.Errorf("the replaced URL: %d %v", w.Code, w.Header())
	}
	w = serveStatic(s, s.URL("css/site.css"))
	if w.Code != 200 || w.Header().Get("Cache-Control") != immutable || w.Body.String() != string(newer) {
		t.Errorf("the new URL: %d %v", w.Code, w.Header())
	}

	for _, name := range []string{"", ".", "/abs.css", "css/../x.css", "css/", "css//x.css"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Add(%q) did not panic", name)
				}
			}()
			s.Add(name, css)
		}()
	}
}

func TestStaticAddWhileServing(t *testing.T) {
	s, err := NewStatic(nil, "/gen/")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if w := serveStatic(s, s.URL("live.css")); w.Code != 200 && w.Code != 404 {
					t.Errorf("%d while adding", w.Code)
				}
			}
		})
	}
	for i := range 100 {
		s.Add("live.css", fmt.Appendf(nil, ".v%d { color: red }", i))
	}
	close(stop)
	wg.Wait()
	if w := serveStatic(s, s.URL("live.css")); w.Body.String() != ".v99 { color: red }" {
		t.Errorf("after the last Add: %q", w.Body)
	}
}

func TestStaticHashLen(t *testing.T) {
	css := []byte(strings.Repeat(".card { color: var(--sb-fg); }\n", 50))
	sum := sha256.Sum256(css)
	hash := hex.EncodeToString(sum[:])
	fsys := fstest.MapFS{"site.css": {Data: css}}
	s, err := NewStatic(fsys, "/s/", StaticHashLen(10))
	if err != nil {
		t.Fatal(err)
	}
	ten := "/s/site." + hash[:10] + ".css"
	if got := s.URL("site.css"); got != ten {
		t.Errorf("URL = %s, want %s", got, ten)
	}
	s.Add("gen.css", css)
	if got := s.URL("gen.css"); got != "/s/gen."+hash[:10]+".css" {
		t.Errorf("an added file's URL = %s", got)
	}
	for path, cache := range map[string]string{
		ten:                             "public, max-age=31536000, immutable",
		"/s/site." + hash[:12] + ".css": "no-cache",
		"/s/site." + hash[:8] + ".css":  "no-cache",
		"/s/site." + hash + ".css":      "no-cache",
		"/s/site.css":                   "no-cache",
	} {
		if w := serveStatic(s, path); w.Code != 200 || w.Header().Get("Cache-Control") != cache || w.Body.String() != string(css) {
			t.Errorf("GET %s: %d %q", path, w.Code, w.Header().Get("Cache-Control"))
		}
	}
	for _, path := range []string{"/s/site." + hash[:7] + ".css", "/s/site." + hash + "0.css", "/s/site.0123456789ABCDEF.css"} {
		if w := serveStatic(s, path); w.Code != 404 {
			t.Errorf("GET %s: %d, want 404", path, w.Code)
		}
	}

	// A Static that names by 12 digits still answers the 10-digit names
	// of URLs made before it.
	def, err := NewStatic(fsys, "/s/")
	if err != nil {
		t.Fatal(err)
	}
	if w := serveStatic(def, ten); w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" || w.Body.String() != string(css) {
		t.Errorf("a 10-digit name from before: %d %v", w.Code, w.Header())
	}

	for _, n := range []int{7, 65, 0, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("StaticHashLen(%d) did not panic", n)
				}
			}()
			StaticHashLen(n)
		}()
	}
}
