package sse

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// discard is a client that takes everything at once.
type discard struct{ header http.Header }

func (d *discard) Header() http.Header         { return d.header }
func (d *discard) WriteHeader(int)             {}
func (d *discard) Write(p []byte) (int, error) { return len(p), nil }
func (d *discard) Flush()                      {}

// frame is a 57 KB page of 500 rows; from one frame to the next, five
// rows change.
func frame(i int) []byte { return table(500, i) }

// table is frame with rows rows, about 114 bytes each.
func table(rows, i int) []byte {
	var b strings.Builder
	b.WriteString(`<main id="app"><table class="board">` + "\n")
	for r := range rows {
		v := r * 7
		if r%100 == i%100 {
			v += i
		}
		fmt.Fprintf(&b, `<tr id="row-%d" class="row"><td class="name">Item %d</td><td class="value">%d</td><td><button data-on:click="@post('/cmd/inc?id=%d')">+</button></td></tr>`+"\n", r, r, v, r)
	}
	b.WriteString("</table></main>")
	return PatchElements([]byte(b.String()))
}

// TestHeapPerStream streams the same 200 frames to 20 streams per
// encoder (5 with the race detector) and measures what each stream keeps
// on the heap.
func TestHeapPerStream(t *testing.T) {
	if testing.Short() {
		t.Skip("streams 200 large frames to many streams")
	}
	const streams, frames = heapStreams, 200
	payload := make([][]byte, frames)
	for i := range payload {
		payload[i] = frame(i)
	}
	for _, c := range []struct {
		name, encoding string
		opt            Options
		maxMiB         float64
	}{
		{"zstd", Zstd, Options{}, 2},
		{"zstd with low memory", Zstd, Options{ZstdLowMemory: true}, 1.5},
		{"zstd at the default level", Zstd, Options{ZstdLevel: zstd.SpeedDefault}, 3.5},
		{"zstd at better compression", Zstd, Options{ZstdLevel: zstd.SpeedBetterCompression}, 7},
		{"brotli", Brotli, Options{}, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := heap()
			open := make([]*Stream, streams)
			for i := range open {
				s, err := Open(&discard{http.Header{}}, request(t.Context(), c.encoding), c.opt)
				if err != nil {
					t.Fatal(err)
				}
				open[i] = s
			}
			start := time.Now()
			for _, f := range payload {
				for _, s := range open {
					if err := s.Write(f); err != nil {
						t.Fatal(err)
					}
				}
			}
			took := time.Since(start)
			perStream := float64(heap()-before) / streams / (1 << 20)
			in, wire := open[0].Sent()
			t.Logf("%s: %.2f MiB per stream, %v per frame, %d bytes per frame on the wire, %.0f:1 (%d bytes of events, %d on the wire)",
				c.name, perStream, took/(streams*frames), wire/frames, float64(in)/float64(wire), in, wire)
			if perStream > c.maxMiB {
				t.Errorf("%.2f MiB per stream, want under %.1f", perStream, c.maxMiB)
			}
			for _, s := range open {
				s.Close()
			}
			runtime.KeepAlive(payload)
		})
	}
}

// TestBrotliHeapWithinTier streams frames of 230 KB, larger than those
// the tiers were measured with, to brotli streams of each tier and checks
// that they hold no more than the tier books, and that the body decodes.
func TestBrotliHeapWithinTier(t *testing.T) {
	if testing.Short() {
		t.Skip("streams large frames to many streams")
	}
	payload := make([][]byte, 20)
	for i := range payload {
		payload[i] = table(1300, i)
	}
	for _, q := range []int{3, 4} {
		for _, tr := range brotliTiers[q] {
			if tr.window > 19 {
				continue
			}
			t.Run(tr.String(), func(t *testing.T) {
				before := heap()
				open := make([]*Stream, heapStreams)
				recs := make([]*httptest.ResponseRecorder, len(open))
				for i := range open {
					recs[i] = httptest.NewRecorder()
					s, err := Open(recs[i], request(t.Context(), Brotli), Options{BrotliQuality: q, BrotliWindow: tr.window})
					if err != nil {
						t.Fatal(err)
					}
					open[i] = s
				}
				for _, f := range payload {
					for _, s := range open {
						if err := s.Write(f); err != nil {
							t.Fatal(err)
						}
					}
				}
				// The recorders keep the bodies: count what the encoders hold.
				var bodies int64
				for _, r := range recs {
					bodies += int64(r.Body.Cap())
				}
				perStream := (heap() - before - bodies) / int64(len(open))
				t.Logf("%s: %d KiB per stream, %d KiB booked", tr, perStream>>10, tr.heap>>10)
				if perStream > tr.heap {
					t.Errorf("%d KiB per stream, more than the %d KiB booked", perStream>>10, tr.heap>>10)
				}
				open[0].Close()
				got, err := io.ReadAll(decoder(t, Brotli, recs[0].Body.Bytes()))
				if err != nil {
					t.Fatal(err)
				}
				if want := bytes.Join(payload, nil); !bytes.Equal(got, want) {
					t.Fatalf("decoded %d bytes, want the %d written", len(got), len(want))
				}
				for _, s := range open[1:] {
					s.Close()
				}
			})
		}
	}
}

// TestBrotliRefersToTheFrameBefore sends a list of 1,300 rows (230 KB)
// in which one row changes before each frame: a brotli stream whose window
// holds the frame before sends each later frame in a few hundred bytes.
func TestBrotliRefersToTheFrameBefore(t *testing.T) {
	for _, q := range []int{3, 4} {
		s, err := Open(&discard{http.Header{}}, request(t.Context(), Brotli), Options{BrotliQuality: q, BrotliWindow: 18})
		if err != nil {
			t.Fatal(err)
		}
		var first int64
		const rows, frames = 1300, 40
		values := make([]int, rows)
		for i := range frames {
			values[i*7919%rows] += i
			var b strings.Builder
			for r, v := range values {
				fmt.Fprintf(&b, `<tr id="row-%d" class="row"><td class="name">Item %d</td><td class="value">%d</td><td><button data-on:click="@post('/cmd/inc?id=%d')">+</button></td></tr>`+"\n", r, r, v, r)
			}
			if err := s.Write(PatchElements([]byte(b.String()))); err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				_, first = s.Sent()
			}
		}
		_, wire := s.Sent()
		perFrame := (wire - first) / (frames - 1)
		t.Logf("q%d 2^18: %d bytes per later frame", q, perFrame)
		if perFrame > 500 {
			t.Errorf("q%d: %d bytes per later frame, want under 500", q, perFrame)
		}
		s.Close()
	}
}

func heap() int64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}
