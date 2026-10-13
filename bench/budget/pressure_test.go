package budget

import (
	"encoding/csv"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sse"
)

// TestPressure measures what lowering a budget's limit does, as a process
// near its memory limit would (Budget.SetLimit): 500 streams of the board
// hold zstd 2^19 in a budget of 1 GiB; the limit drops to 512 MiB, and each
// stream writes its next frame. It reports the reserved bytes, the live
// heap and the bytes on the wire per frame before, at and after the drop,
// and writes pressure.csv.
func TestPressure(t *testing.T) {
	request(t)
	const streams = 500
	frames := boardFrames(t, 5)
	b := sse.NewBudget(sse.BudgetOptions{Limit: 1 << 30, MinWindow: 18, MaxWindow: 19})
	before := liveHeap()
	sinks := make([]*sink, streams)
	open := make([]*sse.Stream, streams)
	for i := range open {
		sinks[i] = &sink{header: http.Header{}}
		s, err := sse.Open(sinks[i], httptestRequest(), sse.Options{Budget: b})
		if err != nil {
			t.Fatal(err)
		}
		open[i] = s
	}
	f, err := os.Create("pressure.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := csv.NewWriter(f)
	c.Write([]string{"step", "limit_mib", "reserved_mib", "live_heap_mib", "bytes_per_frame", "us_per_frame", "tiers"})
	round := func(step string, frame []byte) {
		var wire int64
		start := time.Now()
		for i, s := range open {
			n0 := sinks[i].n
			if err := s.Write(frame); err != nil {
				t.Fatal(err)
			}
			wire += sinks[i].n - n0
		}
		took := time.Since(start)
		st := b.Stats()
		heap := float64(liveHeap()-before) / mib
		var tiers []string
		for _, k := range slices.Sorted(maps.Keys(st.Streams)) {
			tiers = append(tiers, fmt.Sprintf("%s: %d", k, st.Streams[k]))
		}
		c.Write([]string{step, strconv.FormatFloat(float64(st.Limit)/mib, 'f', 0, 64), strconv.FormatFloat(float64(st.Reserved)/mib, 'f', 1, 64),
			strconv.FormatFloat(heap, 'f', 1, 64), strconv.FormatInt(wire/streams, 10),
			strconv.FormatFloat(float64(took)/streams/1e3, 'f', 1, 64), strings.Join(tiers, "; ")})
		t.Logf("%-28s limit %4.0f MiB, reserved %6.1f MiB, live heap %6.1f MiB, %6d B/frame, %5.1f us/frame, %s",
			step, float64(st.Limit)/mib, float64(st.Reserved)/mib, heap, wire/streams, float64(took)/streams/1e3, strings.Join(tiers, "; "))
	}
	round("first frame", frames[0])
	round("second frame", frames[1])
	b.SetLimit(512 << 20)
	round("frame after the drop", frames[2])
	round("next frame", frames[3])
	b.SetLimit(1 << 30)
	round("frame after the limit is back", frames[4])
	for _, s := range open {
		s.Close()
	}
	c.Flush()
}
