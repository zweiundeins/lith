package edge

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

// minSize is the smallest body worth compressing: below it the encoding saves
// about what its headers cost.
const minSize = 512

// Compress compresses the responses to GET, HEAD and QUERY with brotli, zstd or
// gzip, whichever the request accepts best (in that order when it accepts them
// equally). It leaves alone a response that sets its own Content-Encoding
// (precompressed files, streams that compress themselves), event streams,
// partial content, types that don't compress, and bodies under 512 bytes.
// Other methods pass untouched: Datastar's POST streams compress themselves,
// across frames.
//
// A Flush sends everything written so far to the client. The writer unwraps
// for http.ResponseController, so a stream can still set its deadlines.
func Compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		read := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == "QUERY"
		if !read || r.Header.Get("Upgrade") != "" {
			next.ServeHTTP(w, r)
			return
		}
		cw := &compressWriter{w: w, coding: Negotiate(r.Header.Get("Accept-Encoding"))}
		next.ServeHTTP(cw, r)
		cw.close()
	})
}

// compressWriter holds the status and the first bytes of a response until it
// knows enough to choose between compressing it and passing it through.
type compressWriter struct {
	w       http.ResponseWriter
	coding  string // the best the request accepts, "" for none
	status  int
	decided bool
	enc     *encoder // nil: passing through
	buf     []byte
}

func (c *compressWriter) Header() http.Header { return c.w.Header() }

func (c *compressWriter) Unwrap() http.ResponseWriter { return c.w }

func (c *compressWriter) WriteHeader(code int) {
	switch {
	case c.decided:
		c.w.WriteHeader(code) // superfluous, as net/http will say
	case code >= 100 && code < 200 && code != http.StatusSwitchingProtocols:
		c.w.WriteHeader(code) // informational, such as 103 Early Hints
	case c.status == 0:
		c.status = code
	}
}

func (c *compressWriter) Write(p []byte) (int, error) {
	if !c.decided {
		if len(c.buf)+len(p) < minSize {
			if c.buf == nil {
				c.buf = make([]byte, 0, minSize)
			}
			c.buf = append(c.buf, p...)
			return len(p), nil
		}
		if err := c.decide(true, p); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	if c.enc != nil {
		return c.enc.Write(p)
	}
	return c.w.Write(p)
}

func (c *compressWriter) Flush() { c.FlushError() }

func (c *compressWriter) FlushError() error {
	if !c.decided {
		if err := c.decide(true, nil); err != nil {
			return err
		}
	}
	if c.enc != nil {
		if err := c.enc.Flush(); err != nil {
			return err
		}
	}
	return http.NewResponseController(c.w).Flush()
}

// decide writes the header, set up for compression when compress allows and
// the response qualifies, then the body held so far and p.
func (c *compressWriter) decide(compress bool, p []byte) error {
	c.decided = true
	if c.status == 0 {
		c.status = http.StatusOK
	}
	h := c.w.Header()
	if c.qualifies(h, p) {
		addVary(h, "Accept-Encoding")
		if compress && c.coding != "" {
			h.Set("Content-Encoding", c.coding)
			h.Del("Content-Length")
			h.Del("Accept-Ranges")
			if etag := h.Get("ETag"); strings.HasPrefix(etag, `"`) {
				h.Set("ETag", "W/"+etag)
			}
			c.enc = getEncoder(c.coding, c.w)
		}
	}
	c.w.WriteHeader(c.status)
	var out io.Writer = c.w
	if c.enc != nil {
		out = c.enc
	}
	held := c.buf
	c.buf = nil
	if len(held) > 0 {
		if _, err := out.Write(held); err != nil {
			return err
		}
	}
	if len(p) > 0 {
		_, err := out.Write(p)
		return err
	}
	return nil
}

// qualifies reports whether the response would be compressed for a request
// that accepts it. A missing Content-Type is sniffed, as net/http would, from
// the plain bytes.
func (c *compressWriter) qualifies(h http.Header, p []byte) bool {
	switch c.status {
	case http.StatusNoContent, http.StatusPartialContent, http.StatusNotModified:
		return false
	}
	if c.status < 200 || h.Get("Content-Encoding") != "" || h.Get("Content-Range") != "" ||
		hasToken(h.Values("Cache-Control"), "no-transform") {
		return false
	}
	if _, set := h["Content-Type"]; !set {
		head := append(c.buf[:len(c.buf):len(c.buf)], p[:min(len(p), minSize)]...)
		if len(head) == 0 {
			return false
		}
		h.Set("Content-Type", http.DetectContentType(head))
	}
	return compressible(h.Get("Content-Type"))
}

func (c *compressWriter) close() {
	if !c.decided {
		if c.status == 0 && len(c.buf) == 0 {
			return // nothing written: net/http answers, or the handler hijacked
		}
		c.decide(false, nil)
	}
	if c.enc != nil {
		c.enc.Close()
		putEncoder(c.enc)
		c.enc = nil
	}
}

// compressible reports whether a Content-Type is text that compresses.
func compressible(ctype string) bool {
	mt, _, _ := strings.Cut(ctype, ";")
	mt = strings.ToLower(strings.TrimSpace(mt))
	switch {
	case mt == "text/event-stream":
		return false
	case strings.HasPrefix(mt, "text/"), strings.HasSuffix(mt, "+json"), strings.HasSuffix(mt, "+xml"):
		return true
	}
	switch mt {
	case "application/json", "application/javascript", "application/xml", "application/wasm":
		return true
	}
	return false
}

// Negotiate returns the one of codings that an Accept-Encoding header rates
// highest, earlier ones winning ties, or "" when it accepts none of them.
// Without codings it chooses among br, zstd and gzip, as Compress does. A
// handler with prerendered variants names the ones it has, such as br and
// gzip from Precompress.
func Negotiate(acceptEncoding string, codings ...string) string {
	if len(codings) == 0 {
		codings = []string{"br", "zstd", "gzip"}
	}
	best, bestQ := "", 0.0
	if acceptEncoding == "" {
		return best
	}
	for _, coding := range codings {
		if q := quality(acceptEncoding, coding); q > bestQ {
			best, bestQ = coding, q
		}
	}
	return best
}

// quality is the weight an Accept-Encoding header gives coding: its own, else
// the wildcard's, else 0.
func quality(header, coding string) float64 {
	wild := 0.0
	for part := range strings.SplitSeq(header, ",") {
		name, params, _ := strings.Cut(part, ";")
		name = strings.TrimSpace(name)
		q := 1.0
		for param := range strings.SplitSeq(params, ";") {
			k, v, _ := strings.Cut(param, "=")
			if strings.EqualFold(strings.TrimSpace(k), "q") {
				var err error
				if q, err = strconv.ParseFloat(strings.TrimSpace(v), 64); err != nil || q < 0 || q > 1 {
					q = 0
				}
			}
		}
		switch {
		case strings.EqualFold(name, coding), coding == "gzip" && strings.EqualFold(name, "x-gzip"):
			return q
		case name == "*":
			wild = q
		}
	}
	return wild
}

// hasToken reports whether a comma-separated header lists token.
func hasToken(values []string, token string) bool {
	for _, v := range values {
		for part := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// addVary adds field to the Vary header unless it is there, or "*" is.
func addVary(h http.Header, field string) {
	if !hasToken(h.Values("Vary"), field) && !hasToken(h.Values("Vary"), "*") {
		h.Add("Vary", field)
	}
}

// encoder is a pooled compressor. It writes through sink, so that one waiting
// in the pool holds on to no response.
type encoder struct {
	codec interface {
		io.Writer
		Flush() error
		Close() error
		Reset(io.Writer)
	}
	sink   sink
	coding string
}

type sink struct{ w io.Writer }

func (s *sink) Write(p []byte) (int, error) { return s.w.Write(p) }

func (e *encoder) Write(p []byte) (int, error) { return e.codec.Write(p) }
func (e *encoder) Flush() error                { return e.codec.Flush() }
func (e *encoder) Close() error                { return e.codec.Close() }

// With 256 KB windows a writer stays under 5 MB on pages up to 1 MB; the
// defaults took up to 13 MB (brotli) and 18 MB (zstd) for no better ratio.
var encoders = map[string]*sync.Pool{
	"br": {New: func() any {
		e := &encoder{coding: "br"}
		e.codec = brotli.NewWriterOptions(&e.sink, brotli.WriterOptions{Quality: 5, LGWin: 18})
		return e
	}},
	"zstd": {New: func() any {
		e := &encoder{coding: "zstd"}
		e.codec, _ = zstd.NewWriter(&e.sink, zstd.WithEncoderConcurrency(1), zstd.WithLowerEncoderMem(true), zstd.WithWindowSize(1<<18))
		return e
	}},
	"gzip": {New: func() any {
		e := &encoder{coding: "gzip"}
		e.codec, _ = gzip.NewWriterLevel(&e.sink, gzip.DefaultCompression)
		return e
	}},
}

func getEncoder(coding string, w io.Writer) *encoder {
	e := encoders[coding].Get().(*encoder)
	e.sink.w = w
	e.codec.Reset(&e.sink)
	return e
}

func putEncoder(e *encoder) {
	e.sink.w = nil
	encoders[e.coding].Put(e)
}
