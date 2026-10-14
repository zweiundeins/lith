package app_test

import (
	"bytes"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"github.com/zweiundeins/lith/edge"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/web"
)

// TestPrecompressed asks for every static file, the ones the site makes
// and the generated text files, as a client that accepts brotli and as
// one that accepts only gzip. Each compressible one of 512 bytes or more
// must come precompressed, the very bytes of edge.Precompress (brotli -11,
// gzip -9, computed once), not compressed on the fly; fonts and images
// that are compressed already must come as they are.
func TestPrecompressed(t *testing.T) {
	_, base := apptest.Start(t)
	paths := []string{"/static/site.css", "/static/art/logo.svg", "/sitemap.xml", "/robots.txt", "/favicon.ico", "/apple-touch-icon.png", "/og.png"}
	err := fs.WalkDir(web.StaticFiles(), ".", func(name string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			paths = append(paths, "/static/"+name)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	fetch := func(path, accept string) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", base+path, nil)
		req.Header.Set("Accept-Encoding", accept) // set by hand: the transport leaves the body as sent
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
		return res, b
	}
	compressed := 0
	for _, path := range paths {
		res, plain := fetch(path, "identity")
		if enc := res.Header.Get("Content-Encoding"); enc != "" {
			t.Errorf("%s: %s for a client that accepts none", path, enc)
		}
		ctype := res.Header.Get("Content-Type")
		text := strings.HasPrefix(ctype, "text/") || strings.HasPrefix(ctype, "application/json") ||
			strings.HasPrefix(ctype, "application/xml") || strings.HasPrefix(ctype, "image/svg+xml")
		brRes, brBody := fetch(path, "gzip, deflate, br, zstd")
		gzRes, gzBody := fetch(path, "gzip")
		if !text || len(plain) < 512 {
			if enc := brRes.Header.Get("Content-Encoding") + gzRes.Header.Get("Content-Encoding"); enc != "" {
				t.Errorf("%s (%s, %d bytes): sent as %s", path, ctype, len(plain), enc)
			}
			continue
		}
		wantBr, wantGz := edge.Precompress(plain)
		if brRes.Header.Get("Content-Encoding") != "br" || !bytes.Equal(brBody, wantBr) {
			t.Errorf("%s (%s): %q, %d bytes, not brotli -11's %d", path, ctype, brRes.Header.Get("Content-Encoding"), len(brBody), len(wantBr))
		}
		if gzRes.Header.Get("Content-Encoding") != "gzip" || !bytes.Equal(gzBody, wantGz) {
			t.Errorf("%s (%s): %q, %d bytes, not gzip -9's %d", path, ctype, gzRes.Header.Get("Content-Encoding"), len(gzBody), len(wantGz))
		}
		compressed++
	}
	if compressed < 20 {
		t.Errorf("only %d files precompressed", compressed)
	}
}
