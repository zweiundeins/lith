package edge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

// Static serves the files of an fs.FS, and those Add gives it, under a URL
// prefix. URL names a file by its content, as in
// /static/site.3f2a9c1b2d4e.css, and that answer is immutable: browsers keep
// it for a year without asking again. A file asked for by its plain name, or
// by a hash that is no longer its content (a page from before a deploy, or a
// name with another number of hex digits, from 8 to 64), comes with no-cache
// and its ETag, so browsers ask each time and mostly get a 304. Text files go
// out precompressed (see Precompress), compressed in the background from the
// start; other files go out as they are, with ranges. With Minify, its
// JavaScript and CSS go out minified.
type Static struct {
	fsys    fs.FS
	prefix  string
	hashLen int
	minify  func(mediatype string, b []byte) ([]byte, error)

	mu     sync.RWMutex
	files  map[string]*staticFile // by name
	hashed map[string]*staticFile // by hashed name
}

type staticFile struct {
	name, hashed, ctype string
	sum                 [32]byte
	body                []byte // nil: read from fsys when served
	text                bool   // served precompressed
}

// A StaticOption changes how NewStatic names or serves files.
type StaticOption func(*Static)

// StaticHashLen puts n hex digits of a file's SHA-256 in the names URL makes,
// instead of 12. Names an app made the same way before, with that many
// digits, then stay current. It panics unless 8 <= n <= 64.
func StaticHashLen(n int) StaticOption {
	if n < 8 || n > 64 {
		panic(fmt.Sprintf("edge: StaticHashLen(%d) is not between 8 and 64", n))
	}
	return func(s *Static) { s.hashLen = n }
}

// Minify has Static minify its JavaScript and CSS with fn: the files of
// fsys as NewStatic reads them, and those Add gives it. That happens before
// a file is named and compressed, so its URL, its ETag and its
// precompressed forms are all of the minified bytes. fn gets the media
// type, text/javascript or text/css, and the file as written, and returns
// it minified; lith/minify's Bytes is such a function, and so is the Bytes
// method of a tdewolff/minify M. A nil fn minifies nothing.
//
// A file goes out as written when fn fails on it or saves less than a
// twentieth of it, and fn never sees a file named *.min.js or *.min.css:
// code that came minified, such as a vendored Datastar build, stays byte
// for byte the file its integrity hash was made from. Leave Minify out in
// development, for files that the browser's devtools show as written.
func Minify(fn func(mediatype string, b []byte) ([]byte, error)) StaticOption {
	return func(s *Static) { s.minify = fn }
}

// NewStatic reads every file of fsys, to serve them under prefix, such as
// "/static/", and minifies them with Minify's function. Mount it there,
// without http.StripPrefix. fsys may be nil, for a Static that serves only
// what Add gives it.
func NewStatic(fsys fs.FS, prefix string, opts ...StaticOption) (*Static, error) {
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	s := &Static{fsys: fsys, prefix: prefix, hashLen: 12, files: map[string]*staticFile{}, hashed: map[string]*staticFile{}}
	for _, opt := range opts {
		opt(s)
	}
	if fsys == nil {
		return s, nil
	}
	var text []*staticFile
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		f := s.newFile(name, b)
		if f.text {
			text = append(text, f)
		} else {
			f.body = nil
		}
		s.files[name], s.hashed[f.hashed] = f, f
		return nil
	})
	if err != nil {
		return nil, err
	}
	go func() {
		for _, f := range text {
			warm(f)
		}
	}()
	return s, nil
}

// Add serves data as the file name, in place of a file of that name. URL
// names the new content at once, and the URL of the content it replaces
// gets the new one with no-cache, like a hash from before a deploy. With
// Minify, it serves data minified. Add may run while s serves; s keeps data,
// which must not change after. It panics on a name that fs.ValidPath
// refuses.
func (s *Static) Add(name string, data []byte) {
	if !fs.ValidPath(name) || name == "." {
		panic(fmt.Sprintf("edge: Static.Add(%q): not a file name", name))
	}
	f := s.newFile(name, data)
	s.mu.Lock()
	if old := s.files[name]; old != nil {
		delete(s.hashed, old.hashed)
	}
	s.files[name], s.hashed[f.hashed] = f, f
	s.mu.Unlock()
	if f.text {
		go warm(f)
	}
}

func (s *Static) newFile(name string, b []byte) *staticFile {
	ctype := staticType(name, b)
	b = s.minified(name, ctype, b)
	f := &staticFile{name: name, ctype: ctype, sum: sha256.Sum256(b), body: b}
	f.hashed = hashName(name, hex.EncodeToString(f.sum[:])[:s.hashLen])
	f.text = compressible(f.ctype)
	return f
}

// minified returns b as Minify's function minifies it, or b itself.
func (s *Static) minified(name, ctype string, b []byte) []byte {
	mediatype, _, _ := strings.Cut(ctype, ";")
	if s.minify == nil || mediatype != "text/javascript" && mediatype != "text/css" ||
		strings.HasSuffix(strings.TrimSuffix(name, path.Ext(name)), ".min") {
		return b
	}
	m, err := s.minify(mediatype, b)
	if err != nil || len(m) >= len(b) || len(b)-len(m) < len(b)/20 {
		return b
	}
	return m
}

// warm compresses a text file ahead of its first request.
func warm(f *staticFile) {
	if len(f.body) >= minSize {
		precompress(f.body, f.sum)
	}
}

// URL returns the hashed, immutable URL of the named file, or its plain URL
// when there is no such file.
func (s *Static) URL(name string) string {
	s.mu.RLock()
	f := s.files[name]
	s.mu.RUnlock()
	if f != nil {
		return s.prefix + f.hashed
	}
	return s.prefix + name
}

func (s *Static) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name, ok := strings.CutPrefix(r.URL.Path, s.prefix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, current := s.lookup(name)
	if f == nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	if current {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	if f.text {
		serveContent(w, r, f.body, f.sum)
		return
	}
	if f.body != nil {
		h.Set("ETag", `"`+etagOf(f.sum)+`"`)
		http.ServeContent(w, r, f.name, time.Time{}, bytes.NewReader(f.body))
		return
	}
	file, err := s.fsys.Open(f.name)
	if err != nil {
		http.Error(w, "file unavailable", http.StatusInternalServerError)
		return
	}
	defer file.Close()
	rs, ok := file.(io.ReadSeeker)
	if !ok {
		b, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, "file unavailable", http.StatusInternalServerError)
			return
		}
		rs = bytes.NewReader(b)
	}
	h.Set("ETag", `"`+etagOf(f.sum)+`"`)
	http.ServeContent(w, r, f.name, time.Time{}, rs)
}

// lookup finds the file a request names, and whether the name is its current
// hashed one.
func (s *Static) lookup(name string) (f *staticFile, current bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if f = s.hashed[name]; f != nil {
		return f, true
	}
	if f = s.files[name]; f == nil {
		if plain, ok := unhash(name); ok {
			f = s.files[plain]
		}
	}
	return f, false
}

// hashName puts hash before the extension: css/site.css becomes
// css/site.<hash>.css.
func hashName(name, hash string) string {
	dir, file := path.Split(name)
	if i := strings.LastIndexByte(file, '.'); i > 0 {
		return dir + file[:i] + "." + hash + file[i:]
	}
	return dir + file + "." + hash
}

// unhash undoes hashName for any hash.
func unhash(name string) (string, bool) {
	dir, file := path.Split(name)
	last := strings.LastIndexByte(file, '.')
	if last < 0 {
		return "", false
	}
	if isHash(file[last+1:]) {
		return dir + file[:last], true
	}
	prev := strings.LastIndexByte(file[:last], '.')
	if prev < 0 || !isHash(file[prev+1:last]) {
		return "", false
	}
	return dir + file[:prev] + file[last:], true
}

func isHash(s string) bool {
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// staticTypes are the types browsers check, fixed so that the host's
// mime.types cannot change them.
var staticTypes = map[string]string{
	".css":         "text/css; charset=utf-8",
	".html":        "text/html; charset=utf-8",
	".ico":         "image/x-icon",
	".js":          "text/javascript; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".mjs":         "text/javascript; charset=utf-8",
	".svg":         "image/svg+xml",
	".txt":         "text/plain; charset=utf-8",
	".wasm":        "application/wasm",
	".webmanifest": "application/manifest+json",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".xml":         "application/xml",
}

func staticType(name string, b []byte) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := staticTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return http.DetectContentType(b)
}
