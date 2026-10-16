package minify_test

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"net/http/httptest"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/zweiundeins/lith/edge"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/minify"
)

// The static files of the template and of lith's site, which lith's module
// zip leaves out.
var staticDirs = map[string]string{
	"template": "../template/internal/web/static",
	"site":     "../site/internal/web/static",
}

// readDir reads dir into memory, as go:embed holds an app's files.
func readDir(tb testing.TB, dir string) fs.FS {
	tb.Helper()
	files := fstest.MapFS{}
	err := fs.WalkDir(os.DirFS(dir), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path.Join(dir, p))
		files[p] = &fstest.MapFile{Data: b}
		return err
	})
	if errors.Is(err, fs.ErrNotExist) {
		tb.Skipf("%s is not here, as in lith's module zip", dir)
	}
	if err != nil {
		tb.Fatal(err)
	}
	return files
}

// newStatic serves fsys as the template serves its static files, lith's
// newest-frame.js added, with opts.
func newStatic(tb testing.TB, fsys fs.FS, opts ...edge.StaticOption) *edge.Static {
	tb.Helper()
	s, err := edge.NewStatic(fsys, "/static/", opts...)
	if err != nil {
		tb.Fatal(err)
	}
	plugin, err := fs.ReadFile(live.Scripts, live.NewestFrame)
	if err != nil {
		tb.Fatal(err)
	}
	s.Add("lith/"+live.NewestFrame, plugin)
	return s
}

// TestStatic serves the template's and the site's static files with
// Minify(minify.Bytes): their own scripts and stylesheets go out minified,
// and the vendored builds byte for byte as written, as their integrity
// hashes need; lith's newest-frame.js, a minified build too, gains too
// little to change. Run with -v, it lists the sizes, raw and under brotli -11.
func TestStatic(t *testing.T) {
	for name, dir := range staticDirs {
		t.Run(name, func(t *testing.T) {
			fsys := readDir(t, dir)
			s := newStatic(t, fsys, edge.Minify(minify.Bytes))
			files := map[string][]byte{}
			fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					files[p], err = fs.ReadFile(fsys, p)
				}
				return err
			})
			files["lith/"+live.NewestFrame], _ = fs.ReadFile(live.Scripts, live.NewestFrame)
			var written, served, writtenBr, servedBr int
			for _, p := range slices.Sorted(maps.Keys(files)) {
				b := files[p]
				r := httptest.NewRequest("GET", s.URL(p), nil)
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				got := w.Body.Bytes()
				ext := path.Ext(p)
				switch {
				case strings.HasPrefix(p, "vendor/") || p == "lith/"+live.NewestFrame || ext != ".js" && ext != ".css":
					if !bytes.Equal(got, b) {
						t.Errorf("%s: %d bytes served, %d as written", p, len(got), len(b))
					}
					if ext != ".js" && ext != ".css" {
						continue
					}
				default:
					mt := "text/css"
					if ext == ".js" {
						mt = "text/javascript"
					}
					want, err := minify.Bytes(mt, b)
					if err != nil || !bytes.Equal(got, want) || len(got) >= len(b) {
						t.Errorf("%s: %d bytes served, %d as written, %d minified (%v)", p, len(got), len(b), len(want), err)
					}
				}
				wb, gb := edge.BrotliLen(b), edge.BrotliLen(got)
				written, served, writtenBr, servedBr = written+len(b), served+len(got), writtenBr+wb, servedBr+gb
				t.Logf("%-50s %7d %7d   brotli %6d %6d", p, len(b), len(got), wb, gb)
			}
			t.Logf("%-50s %7d %7d   brotli %6d %6d", "JavaScript and CSS", written, served, writtenBr, servedBr)
		})
	}
}

// BenchmarkNewStatic reads the template's and the site's static files with
// and without Minify, as their servers start.
func BenchmarkNewStatic(b *testing.B) {
	for _, name := range []string{"template", "site"} {
		for _, mode := range []string{"as-written", "minified"} {
			b.Run(name+"/"+mode, func(b *testing.B) {
				var opts []edge.StaticOption
				if mode == "minified" {
					opts = append(opts, edge.Minify(minify.Bytes))
				}
				fsys := readDir(b, staticDirs[name])
				for b.Loop() {
					newStatic(b, fsys, opts...)
				}
			})
		}
	}
}
