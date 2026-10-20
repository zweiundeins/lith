package edge

import (
	"bytes"
	"fmt"
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
		"gzip, deflate, br, zstd": "zstd",
		"gzip, deflate, br":       "br",
		"zstd":                    "zstd",
		"gzip":                    "gzip",
		"x-gzip":                  "gzip",
		"br;q=0, gzip":            "gzip",
		"gzip;q=1, br;q=0.5":      "gzip",
		"zstd;q=0.9, br;q=0.9":    "zstd",
		"zstd;q=0.8, br;q=0.9":    "br",
		"*":                       "zstd",
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
		{"gzip, deflate, br, zstd", nil, "zstd"},
		{"gzip, deflate, br", nil, "br"},
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

	// QUERY is a read, like GET: @query's answers compress.
	w = serveCompressed(html(page), "QUERY", "br")
	if w.Header().Get("Content-Encoding") != "br" || !bytes.Equal(decode(t, "br", w.Body.Bytes()), page) {
		t.Errorf("QUERY: %v", w.Header())
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

// boardPage is a page of about 150 KB like a kanban board's: lists of
// cards with titles, labels and Datastar attributes.
func boardPage() []byte {
	words := strings.Fields("release plan review design copy fix bug invoice client meeting draft deploy sprint backlog migrate test docs search export import mobile layout cache report budget")
	var b bytes.Buffer
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Board</title></head><body><main id="board" class="board">`)
	x := uint32(1)
	next := func(n int) int { x ^= x << 13; x ^= x >> 17; x ^= x << 5; return int(x % uint32(n)) }
	for list := 0; b.Len() < 150_000; list++ {
		fmt.Fprintf(&b, `<section class="list" id="list-%d"><h2 class="list__title">List %d</h2><ol class="list__cards">`, list, list)
		for card := range 40 {
			id := list*100 + card
			fmt.Fprintf(&b, `<li class="card" id="card-%d" draggable="true" data-on:click="@post('/cmd/open?card=%d')" data-on:dragstart="$drag = %d">`, id, id, id)
			fmt.Fprintf(&b, `<span class="card__title">%s %s %s</span>`, words[next(len(words))], words[next(len(words))], words[next(len(words))])
			fmt.Fprintf(&b, `<span class="card__label card__label--%d">%s</span><time datetime="2026-10-%02d">Oct %d</time></li>`, next(6), words[next(len(words))], 1+next(28), 1+next(28))
		}
		b.WriteString(`</ol></section>`)
	}
	b.WriteString(`</main></body></html>`)
	return b.Bytes()
}

type discard struct{ h http.Header }

func (d *discard) Header() http.Header         { return d.h }
func (d *discard) Write(p []byte) (int, error) { return len(p), nil }
func (d *discard) WriteHeader(int)             {}

// BenchmarkCompress compresses a 150 KB page with each coding, as
// Compress does for every request that accepts it.
//
//	go test ./edge -run '^$' -bench Compress -count 10
func BenchmarkCompress(b *testing.B) {
	body := boardPage()
	h := Compress(html(body))
	for _, coding := range []string{"br", "zstd", "gzip"} {
		b.Run(coding, func(b *testing.B) {
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("Accept-Encoding", coding)
			out := httptest.NewRecorder()
			h.ServeHTTP(out, r)
			if got := out.Header().Get("Content-Encoding"); got != coding {
				b.Fatalf("Content-Encoding %q", got)
			}
			w := &discard{h: http.Header{}}
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				clear(w.h)
				h.ServeHTTP(w, r)
			}
			b.ReportMetric(100*float64(out.Body.Len())/float64(len(body)), "%-of-input")
		})
	}
}
