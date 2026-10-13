package edge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"hash/maphash"
	"math/bits"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
)

type precompressed struct {
	once   sync.Once
	br, gz []byte
}

var precompressCache sync.Map // sha256 of the plain bytes to *precompressed

// Precompress returns b under brotli -11 and gzip -9, compressed once per
// content: callers with the same bytes wait for the one compression and later
// ones get it at once. The cache keeps every content for the life of the
// process, so it is for bytes served many times: static files and generated
// assets.
func Precompress(b []byte) (br, gz []byte) {
	return precompress(b, sha256.Sum256(b))
}

func precompress(b []byte, sum [32]byte) (br, gz []byte) {
	v, ok := precompressCache.Load(sum)
	if !ok {
		v, _ = precompressCache.LoadOrStore(sum, new(precompressed))
	}
	p := v.(*precompressed)
	p.once.Do(func() {
		var out bytes.Buffer
		// A window just larger than the input compresses as well as the 4 MB
		// default and keeps the encoder's memory small.
		win := max(10, min(24, bits.Len(uint(len(b)))+1))
		bw := brotli.NewWriterOptions(&out, brotli.WriterOptions{Quality: brotli.BestCompression, LGWin: win})
		bw.Write(b)
		bw.Close()
		p.br = bytes.Clone(out.Bytes())
		out.Reset()
		gw, _ := gzip.NewWriterLevel(&out, gzip.BestCompression)
		gw.Write(b)
		gw.Close()
		p.gz = out.Bytes()
	})
	return p.br, p.gz
}

// ServePrecompressed answers with body in the best of Precompress's codings
// the request accepts. Bodies under 512 bytes, types that don't compress and
// requests that accept neither coding get it as it is. It sets an ETag of the
// content and leaves conditional and range requests to http.ServeContent, so
// an If-None-Match that names the ETag gets a 304. Set Content-Type and
// Cache-Control before; a Content-Encoding it sets keeps Compress out of the
// way.
//
// Like Precompress, it hashes and compresses each content once and keeps the
// result, the body included, for the life of the process: it is for bytes
// served many times, such as prerendered pages.
func ServePrecompressed(w http.ResponseWriter, r *http.Request, body []byte) {
	serveContent(w, r, body, bodySum(body))
}

var (
	bodySeed = maphash.MakeSeed()
	bodySums sync.Map // maphash of a body to *hashedBody
)

type hashedBody struct {
	body []byte
	sum  [32]byte
}

// bodySum is sha256.Sum256(body), looked up by a hash several times faster.
func bodySum(body []byte) [32]byte {
	key := maphash.Bytes(bodySeed, body)
	if v, ok := bodySums.Load(key); ok {
		if s := v.(*hashedBody); bytes.Equal(s.body, body) {
			return s.sum
		}
	}
	s := &hashedBody{body: body, sum: sha256.Sum256(body)}
	bodySums.Store(key, s)
	return s.sum
}

func serveContent(w http.ResponseWriter, r *http.Request, body []byte, sum [32]byte) {
	h := w.Header()
	if _, set := h["Content-Type"]; !set {
		h.Set("Content-Type", http.DetectContentType(body))
	}
	tag, content := etagOf(sum), body
	if len(body) >= minSize && compressible(h.Get("Content-Type")) {
		addVary(h, "Accept-Encoding")
		switch Negotiate(r.Header.Get("Accept-Encoding"), "br", "gzip") {
		case "br":
			if br, _ := precompress(body, sum); len(br) < len(body) {
				tag, content = tag+"-br", br
				h.Set("Content-Encoding", "br")
			}
		case "gzip":
			if _, gz := precompress(body, sum); len(gz) < len(body) {
				tag, content = tag+"-gz", gz
				h.Set("Content-Encoding", "gzip")
			}
		}
	}
	h.Set("ETag", `"`+tag+`"`)
	// ServeContent leaves the length out when Content-Encoding is set.
	h.Set("Content-Length", strconv.Itoa(len(content)))
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(content))
}

func etagOf(sum [32]byte) string { return hex.EncodeToString(sum[:8]) }
