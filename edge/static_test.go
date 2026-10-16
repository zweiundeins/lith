package edge

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// squeeze is a stand-in minifier: it drops the spaces and newlines, and
// records what it was given.
type squeeze struct {
	mu    sync.Mutex
	given map[string]string // a file's first line to the media type it came with
}

func (q *squeeze) minify(mediatype string, b []byte) ([]byte, error) {
	q.mu.Lock()
	if q.given == nil {
		q.given = map[string]string{}
	}
	first, _, _ := bytes.Cut(b, []byte("\n"))
	q.given[string(first)] = mediatype
	q.mu.Unlock()
	switch {
	case bytes.HasPrefix(b, []byte("// broken")):
		return nil, errors.New("unexpected token")
	case bytes.HasPrefix(b, []byte("// tight")):
		return b[:len(b)-len(b)/25], nil // 4% smaller
	}
	return squeezed(b), nil
}

func squeezed(b []byte) []byte {
	return []byte(strings.NewReplacer(" ", "", "\n", "").Replace(string(b)))
}

func source(first string) []byte {
	return []byte(first + "\n" + strings.Repeat("const answer = 42;\n", 60))
}

func TestStaticMinify(t *testing.T) {
	var q squeeze
	css := []byte("/* site */\n" + strings.Repeat(".card { color: red; }\n", 60))
	files := fstest.MapFS{
		"js/app.js":          {Data: source("// app")},
		"js/module.mjs":      {Data: source("// module")},
		"css/site.css":       {Data: css},
		"vendor/lib.min.js":  {Data: source("// vendored")},
		"vendor/lib.min.css": {Data: source("/* vendored css */")},
		"js/broken.js":       {Data: source("// broken")},
		"js/tight.js":        {Data: source("// tight")},
		"data.json":          {Data: source(`{"json": 1}`)},
		"img/logo.png":       {Data: png},
		"js/empty.js":        {Data: nil},
		"js/minified.js":     {Data: []byte("x=1;")},
		"vendor/datastar.js": {Data: source("// datastar")},
		"robots.txt":         {Data: []byte("User-agent: *\n")},
		"css/print.CSS":      {Data: []byte("/* print */\n" + strings.Repeat("body { margin: 0; }\n", 40))},
		"js/a.min.bundle.js": {Data: source("// min mid-name")},
		"js/datastar-min.js": {Data: source("// dash min")},
	}
	s, err := NewStatic(files, "/static/", Minify(q.minify))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{
		"js/app.js":          squeezed(source("// app")),
		"js/module.mjs":      squeezed(source("// module")),
		"css/site.css":       squeezed(css),
		"css/print.CSS":      squeezed(files["css/print.CSS"].Data),
		"js/a.min.bundle.js": squeezed(source("// min mid-name")),
		"js/datastar-min.js": squeezed(source("// dash min")),
		"vendor/datastar.js": squeezed(source("// datastar")),
		// As written: named minified, failed, saved too little, not JS or CSS.
		"vendor/lib.min.js":  source("// vendored"),
		"vendor/lib.min.css": source("/* vendored css */"),
		"js/broken.js":       source("// broken"),
		"js/tight.js":        source("// tight"),
		"js/minified.js":     []byte("x=1;"),
		"js/empty.js":        nil,
		"data.json":          source(`{"json": 1}`),
		"img/logo.png":       png,
	} {
		sum := sha256.Sum256(want)
		url := s.URL(name)
		if hash := hex.EncodeToString(sum[:])[:12]; !strings.Contains(url, "."+hash+".") {
			t.Errorf("%s: URL %s, want the hash %s of the bytes served", name, url, hash)
		}
		w := serveStatic(s, url)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), want) || w.Header().Get("ETag") != `"`+etagOf(sum)+`"` {
			t.Errorf("%s: %d, ETag %s, %d bytes, want %d", name, w.Code, w.Header().Get("ETag"), w.Body.Len(), len(want))
		}
	}
	for first, mediatype := range map[string]string{"// app": "text/javascript", "// module": "text/javascript", "/* site */": "text/css", "/* print */": "text/css", "// broken": "text/javascript"} {
		if q.given[first] != mediatype {
			t.Errorf("%s: the minifier got %q, want %s", first, q.given[first], mediatype)
		}
	}
	for _, first := range []string{"// vendored", "/* vendored css */", `{"json": 1}`, "User-agent: *"} {
		if mediatype, ok := q.given[first]; ok {
			t.Errorf("%s: the minifier got it as %s", first, mediatype)
		}
	}

	// The precompressed forms, a 304 and a range are of the minified bytes.
	want := squeezed(source("// app"))
	sum := sha256.Sum256(want)
	url := s.URL("js/app.js")
	w := serveStatic(s, url, "Accept-Encoding", "br")
	if w.Header().Get("Content-Encoding") != "br" || !bytes.Equal(decode(t, "br", w.Body.Bytes()), want) || w.Header().Get("ETag") != `"`+etagOf(sum)+`-br"` {
		t.Errorf("brotli: %v", w.Header())
	}
	if br, _ := Precompress(want); !bytes.Equal(w.Body.Bytes(), br) {
		t.Error("brotli: not the precompressed form of the minified bytes")
	}
	w = serveStatic(s, url, "Accept-Encoding", "gzip")
	if w.Header().Get("Content-Encoding") != "gzip" || !bytes.Equal(decode(t, "gzip", w.Body.Bytes()), want) {
		t.Errorf("gzip: %v", w.Header())
	}
	for _, path := range []string{url, "/static/js/app.js", "/static/js/app.000000000000.js"} {
		if w := serveStatic(s, path, "If-None-Match", `"`+etagOf(sum)+`"`); w.Code != http.StatusNotModified {
			t.Errorf("%s with the minified bytes' ETag: %d", path, w.Code)
		}
		if w := serveStatic(s, path); !bytes.Equal(w.Body.Bytes(), want) {
			t.Errorf("%s: not the minified bytes", path)
		}
	}
	written := sha256.Sum256(source("// app"))
	if w := serveStatic(s, url, "If-None-Match", `"`+etagOf(written)+`"`); w.Code != 200 {
		t.Errorf("the ETag of the file as written: %d", w.Code)
	}
	w = serveStatic(s, url, "Range", "bytes=5-14")
	if w.Code != http.StatusPartialContent || w.Body.String() != string(want[5:15]) {
		t.Errorf("a range: %d %q, want %q", w.Code, w.Body, want[5:15])
	}
	w = serveStatic(s, s.URL("img/logo.png"), "Range", "bytes=0-3")
	if w.Code != http.StatusPartialContent || w.Body.String() != "\x89PNG" {
		t.Errorf("a range of the png: %d %q", w.Code, w.Body)
	}
}

func TestStaticAddMinifies(t *testing.T) {
	var q squeeze
	s, err := NewStatic(nil, "/gen/", Minify(q.minify))
	if err != nil {
		t.Fatal(err)
	}
	bundle := []byte("/* bundle */\n" + strings.Repeat(".card { color: red; }\n", 60))
	s.Add("site.css", bundle)
	s.Add("lith/plugin.js", source("// plugin"))
	s.Add("vendor/lib.min.js", source("// vendored"))
	s.Add("broken.js", source("// broken"))
	for name, want := range map[string][]byte{
		"site.css":          squeezed(bundle),
		"lith/plugin.js":    squeezed(source("// plugin")),
		"vendor/lib.min.js": source("// vendored"),
		"broken.js":         source("// broken"),
	} {
		sum := sha256.Sum256(want)
		url := s.URL(name)
		w := serveStatic(s, url, "Accept-Encoding", "br")
		if !strings.Contains(url, "."+hex.EncodeToString(sum[:])[:12]+".") || !bytes.Equal(decode(t, "br", w.Body.Bytes()), want) {
			t.Errorf("%s: URL %s, %d bytes, want %d", name, url, len(decode(t, "br", w.Body.Bytes())), len(want))
		}
	}

	// Without Minify, or with a nil function, files go out as written.
	for _, opts := range [][]StaticOption{nil, {Minify(nil)}} {
		s, err := NewStatic(fstest.MapFS{"app.js": {Data: source("// app")}}, "/s/", opts...)
		if err != nil {
			t.Fatal(err)
		}
		s.Add("site.css", bundle)
		if w := serveStatic(s, s.URL("app.js")); w.Body.String() != string(source("// app")) {
			t.Errorf("%d options: app.js was changed", len(opts))
		}
		if w := serveStatic(s, s.URL("site.css")); w.Body.String() != string(bundle) {
			t.Errorf("%d options: the added CSS was changed", len(opts))
		}
	}
}
