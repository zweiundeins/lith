package sse

import (
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

// discard is a client that takes everything at once.
type discard struct{ header http.Header }

func (d *discard) Header() http.Header         { return d.header }
func (d *discard) WriteHeader(int)             {}
func (d *discard) Write(p []byte) (int, error) { return len(p), nil }
func (d *discard) Flush()                      {}

// frame is a 57 KB page of 500 rows; from one frame to the next, five
// rows change.
func frame(i int) []byte {
	var b strings.Builder
	b.WriteString(`<main id="app"><table class="board">` + "\n")
	for r := range 500 {
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
		encoding string
		maxMiB   float64
	}{
		{Zstd, 1.5},
		{Brotli, 3},
	} {
		t.Run(c.encoding, func(t *testing.T) {
			before := heap()
			open := make([]*Stream, streams)
			for i := range open {
				s, err := Open(&discard{http.Header{}}, request(t.Context(), c.encoding), Options{})
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
			t.Logf("%s: %.2f MiB per stream, %v per frame, %.0f:1 (%d bytes of events, %d on the wire)",
				c.encoding, perStream, took/(streams*frames), float64(in)/float64(wire), in, wire)
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

func heap() int64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}
