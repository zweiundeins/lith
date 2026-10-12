package edge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// Static serves the files of an fs.FS under a URL prefix. URL names a file by
// its content, as in /static/site.3f2a9c1b2d4e.css, and that answer is
// immutable: browsers keep it for a year without asking again. A file asked
// for by its plain name, or by a hash that is no longer its content (a page
// from before a deploy), comes with no-cache and its ETag, so browsers ask
// each time and mostly get a 304. Text files go out precompressed (see
// Precompress), compressed in the background from the start; other files go
// out as they are, with ranges.
type Static struct {
	fsys   fs.FS
	prefix string
	files  map[string]*staticFile // by name
	hashed map[string]*staticFile // by hashed name
}

type staticFile struct {
	name, hashed, ctype string
	sum                 [32]byte
	body                []byte // text, kept to serve precompressed
}

// NewStatic reads every file of fsys, to serve them under prefix, such as
// "/static/".
func NewStatic(fsys fs.FS, prefix string) (*Static, error) {
	s := &Static{fsys: fsys, prefix: prefix, files: map[string]*staticFile{}, hashed: map[string]*staticFile{}}
	var text []*staticFile
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		f := &staticFile{name: name, ctype: staticType(name, b), sum: sha256.Sum256(b)}
		f.hashed = hashName(name, hex.EncodeToString(f.sum[:6]))
		if compressible(f.ctype) {
			f.body = b
			text = append(text, f)
		}
		s.files[name], s.hashed[f.hashed] = f, f
		return nil
	})
	if err != nil {
		return nil, err
	}
	go func() {
		for _, f := range text {
			if len(f.body) >= minSize {
				precompress(f.body, f.sum)
			}
		}
	}()
	return s, nil
}

// URL is the immutable address of the named file, or its plain one when there
// is no such file.
func (s *Static) URL(name string) string {
	if f, ok := s.files[name]; ok {
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
	f, current := s.hashed[name], true
	if f == nil {
		current = false
		if f = s.files[name]; f == nil {
			if plain, ok := unhash(name); ok {
				f = s.files[plain]
			}
		}
	}
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
	if f.body != nil {
		serveContent(w, r, f.body, f.sum)
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
	if len(s) != 12 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// staticTypes pins the types that matter to browsers, rather than leave them
// to the host's mime.types.
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
