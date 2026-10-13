package edge

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

var page = []byte(strings.Repeat(`<li class="card" data-on:click="@post('/cmd/open')">A card on the board</li>`+"\n", 50))

func decode(t *testing.T, coding string, body []byte) []byte {
	t.Helper()
	var r io.Reader = bytes.NewReader(body)
	switch coding {
	case "br":
		r = brotli.NewReader(r)
	case "gzip":
		zr, err := gzip.NewReader(r)
		if err != nil {
			t.Fatal(err)
		}
		r = zr
	case "zstd":
		zr, err := zstd.NewReader(r)
		if err != nil {
			t.Fatal(err)
		}
		defer zr.Close()
		r = zr
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("decoding %s: %v", coding, err)
	}
	return out
}

func serveCompressed(h http.HandlerFunc, method, accept string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/", nil)
	if accept != "" {
		r.Header.Set("Accept-Encoding", accept)
	}
	w := httptest.NewRecorder()
	Compress(h).ServeHTTP(w, r)
	return w
}

func html(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(body)
	}
}

func TestCompressNegotiates(t *testing.T) {
	for accept, want := range map[string]string{
		"gzip, deflate, br, zstd": "br",
		"zstd":                    "zstd",
		"gzip":                    "gzip",
		"x-gzip":                  "gzip",
		"br;q=0, gzip":            "gzip",
		"gzip;q=1, br;q=0.5":      "gzip",
		"zstd;q=0.9, br;q=0.9":    "br",
		"*":                       "br",
		"*;q=0, gzip":             "gzip",
		"deflate":                 "",
		"br;q=0.0, zstd;q=0":      "",
		"br;q=nonsense":           "",
		"":                        "",
	} {
		for range 2 { // the second round gets a pooled encoder
			w := serveCompressed(html(page), "GET", accept)
			if got := w.Header().Get("Content-Encoding"); got != want {
				t.Errorf("Accept-Encoding %q: %q, want %q", accept, got, want)
				continue
			}
			if got := decode(t, want, w.Body.Bytes()); !bytes.Equal(got, page) {
				t.Errorf("Accept-Encoding %q: the body did not survive", accept)
			}
			if w.Header().Get("Vary") != "Accept-Encoding" {
				t.Errorf("Accept-Encoding %q: Vary %q", accept, w.Header().Values("Vary"))
			}
		}
	}
}

func TestNegotiate(t *testing.T) {
	for _, c := range []struct {
		accept  string
		codings []string
		want    string
	}{
		{"gzip, deflate, br, zstd", nil, "br"},
		{"zstd", nil, "zstd"},
		{"zstd, gzip", nil, "zstd"},
		{"gzip;q=1, br;q=0.5", nil, "gzip"},
		{"*;q=0, x-gzip", nil, "gzip"},
		{"deflate", nil, ""},
		{"", nil, ""},
		{"zstd, gzip", []string{"br", "gzip"}, "gzip"},
		{"zstd", []string{"br", "gzip"}, ""},
		{"gzip, deflate, br, zstd", []string{"zstd", "br"}, "zstd"},
	} {
		if got := Negotiate(c.accept, c.codings...); got != c.want {
			t.Errorf("Negotiate(%q, %q) = %q, want %q", c.accept, c.codings, got, c.want)
		}
	}
}

func TestCompressHeaders(t *testing.T) {
	w := serveCompressed(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Content-Length", "4000")
		h.Set("ETag", `"v1"`)
		h.Set("Accept-Ranges", "bytes")
		h.Add("Vary", "Cookie")
		for range 10 { // small writes add up
			w.Write(page[:400])
		}
	}, "GET", "br")
	h := w.Header()
	if h.Get("Content-Encoding") != "br" || h.Get("Content-Length") != "" || h.Get("Accept-Ranges") != "" || h.Get("ETag") != `W/"v1"` {
		t.Errorf("headers of a compressed response: %v", h)
	}
	if vary := h.Values("Vary"); len(vary) != 2 || vary[0] != "Cookie" || vary[1] != "Accept-Encoding" {
		t.Errorf("Vary %q, want Cookie kept and Accept-Encoding added", vary)
	}
	if got := decode(t, "br", w.Body.Bytes()); !bytes.Equal(got, bytes.Repeat(page[:400], 10)) {
		t.Error("the body did not survive")
	}

	// No Content-Type: sniffed from the plain bytes, as net/http would.
	w = serveCompressed(func(w http.ResponseWriter, r *http.Request) { w.Write(append([]byte("<!doctype html>"), page...)) }, "GET", "gzip")
	if w.Header().Get("Content-Type") != "text/html; charset=utf-8" || w.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("sniffed: %v", w.Header())
	}

	// HEAD gets the headers GET would.
	w = serveCompressed(html(page), "HEAD", "br")
	if w.Header().Get("Content-Encoding") != "br" {
		t.Errorf("HEAD: %v", w.Header())
	}
}

func TestCompressPassesThrough(t *testing.T) {
	for _, c := range []struct {
		name, method, accept string
		handler              http.HandlerFunc
		vary                 bool
	}{
		{"POST", "POST", "br", html(page), false},
		{"WebSocket upgrade", "GET", "br", html(page), false},
		{"event stream", "GET", "br", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write(page)
		}, false},
		{"own Content-Encoding", "GET", "br", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(page)
		}, false},
		{"small body", "GET", "br", html(page[:minSize-1]), true},
		{"image", "GET", "br", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write(page)
		}, false},
		{"partial content", "GET", "br", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Range", "bytes 0-3999/8000")
			w.WriteHeader(http.StatusPartialContent)
			w.Write(page)
		}, false},
		{"no-transform", "GET", "br", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Cache-Control", "public, no-transform")
			w.Write(page)
		}, false},
		{"not accepted", "GET", "", html(page), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, "/", nil)
			r.Header.Set("Accept-Encoding", c.accept)
			if c.name == "WebSocket upgrade" {
				r.Header.Set("Upgrade", "websocket")
			}
			w := httptest.NewRecorder()
			Compress(c.handler).ServeHTTP(w, r)
			if ce := w.Header().Get("Content-Encoding"); ce != "" && c.name != "own Content-Encoding" {
				t.Errorf("compressed with %s", ce)
			}
			if !bytes.Equal(w.Body.Bytes(), page[:len(w.Body.Bytes())]) || w.Body.Len() == 0 {
				t.Error("the body changed")
			}
			if got := w.Header().Get("Vary") == "Accept-Encoding"; got != c.vary {
				t.Errorf("Vary %q", w.Header().Values("Vary"))
			}
		})
	}
}

func TestCompressFlushes(t *testing.T) {
	for _, c := range []struct{ ctype, coding string }{{"text/html", "br"}, {"text/html", "zstd"}, {"text/html", "gzip"}, {"text/event-stream", ""}} {
		chunk := []byte("<p>the head of a page</p>")
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Accept-Encoding", c.coding)
		Compress(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			rw.Header().Set("Content-Type", c.ctype)
			rw.Write(chunk)
			if err := http.NewResponseController(rw).Flush(); err != nil {
				t.Fatal(err)
			}
			if !w.Flushed {
				t.Errorf("%s %s: the flush did not reach the client", c.ctype, c.coding)
			}
			// What arrived so far decodes to the chunk.
			var dec io.Reader = bytes.NewReader(w.Body.Bytes())
			switch c.coding {
			case "br":
				dec = brotli.NewReader(dec)
			case "gzip":
				dec, _ = gzip.NewReader(dec)
			case "zstd":
				zr, _ := zstd.NewReader(dec)
				defer zr.Close()
				dec = zr
			}
			got := make([]byte, len(chunk))
			if _, err := io.ReadFull(dec, got); err != nil || !bytes.Equal(got, chunk) {
				t.Errorf("%s %s: flushed %q, %v", c.ctype, c.coding, got, err)
			}
			rw.Write(page)
		})).ServeHTTP(w, r)
		if got := decode(t, c.coding, w.Body.Bytes()); !bytes.Equal(got, append(chunk, page...)) {
			t.Errorf("%s %s: the whole body did not survive", c.ctype, c.coding)
		}
	}
}

func TestCompressReachesTheConnection(t *testing.T) {
	srv := httptest.NewServer(Compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
			t.Errorf("SetWriteDeadline through Compress: %v", err)
		}
		html(page)(w, r)
	})))
	defer srv.Close()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			req, _ := http.NewRequest("GET", srv.URL, nil)
			req.Header.Set("Accept-Encoding", "br")
			res, err := srv.Client().Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer res.Body.Close()
			body, _ := io.ReadAll(res.Body)
			if res.Header.Get("Content-Encoding") != "br" || !bytes.Equal(decode(t, "br", body), page) {
				t.Errorf("over the wire: %v", res.Header)
			}
		})
	}
	wg.Wait()
}

func TestCompressStatus(t *testing.T) {
	w := serveCompressed(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		w.Write(page)
	}, "GET", "br")
	if w.Code != http.StatusNotFound || w.Header().Get("Content-Encoding") != "br" {
		t.Errorf("a 404 page: %d %v", w.Code, w.Header())
	}
	w = serveCompressed(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotModified)
	}, "GET", "br")
	if w.Code != http.StatusNotModified || w.Header().Get("Content-Encoding") != "" || w.Header().Get("Vary") != "" {
		t.Errorf("a 304: %d %v", w.Code, w.Header())
	}
	w = serveCompressed(func(w http.ResponseWriter, r *http.Request) {}, "GET", "br")
	if w.Code != http.StatusOK || len(w.Header()) != 0 {
		t.Errorf("an empty answer: %d %v", w.Code, w.Header())
	}
}
