package budget

import (
	"bytes"
	"encoding/csv"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"

	"github.com/zweiundeins/lith/sse"
)

// lifeFrames is n generations of a Game of Life of 80 by 50 cells from a
// soup, rendered as lith's site renders its board: rows with ids, a <p>
// of one letter's class per cell, a dying cell and its trail as two more
// letters. Each generation changes a few dozen cells of 4,000.
func lifeFrames(n int) [][]byte {
	const w, h = 80, 50
	rnd := rand.New(rand.NewPCG(5, 8))
	cells := make([]byte, w*h)
	for i := range cells {
		if rnd.IntN(3) == 0 {
			cells[i] = 1
		}
	}
	age := make([]byte, w*h) // generations since a cell died, up to 2
	frames := make([][]byte, n)
	for f := range frames {
		if f > 0 {
			next := make([]byte, w*h)
			for y := range h {
				for x := range w {
					k := 0
					for dy := -1; dy <= 1; dy++ {
						for dx := -1; dx <= 1; dx++ {
							if (dx != 0 || dy != 0) && cells[(y+dy+h)%h*w+(x+dx+w)%w] != 0 {
								k++
							}
						}
					}
					i := y*w + x
					if k == 3 || k == 2 && cells[i] != 0 {
						next[i] = 1
					}
				}
			}
			for i := range age {
				switch {
				case next[i] != 0:
					age[i] = 0
				case cells[i] != 0:
					age[i] = 1
				case age[i] == 1:
					age[i] = 2
				default:
					age[i] = 0
				}
			}
			cells = next
		}
		var b bytes.Buffer
		b.WriteString(`<div id="life-board" class="life__board" aria-hidden="true">`)
		for y := range h {
			b.WriteString(`<div id="life-r` + strconv.Itoa(y) + `">`)
			for x := range w {
				class := byte('_')
				if i := y*w + x; cells[i] != 0 {
					class = 'w'
				} else if age[i] > 0 {
					class = "_Wx"[age[i]]
				}
				b.WriteString("<p class=")
				b.WriteByte(class)
				b.WriteByte('>')
			}
			b.WriteString("</div>")
		}
		b.WriteString("</div>")
		frames[f] = sse.PatchElements(b.Bytes())
	}
	return frames
}

// slowSettings are what TestSlowLinks compares on a workload whose largest
// frame is n bytes: zstd at its fastest and at its default level at every
// zstd tier of the budget's menu, and brotli at quality 3 and 4 at the
// smallest window that holds the frame, the one the budget gives.
func slowSettings(n int) []setting {
	var s []setting
	for _, z := range []setting{{coding: sse.Zstd, window: 20}, {coding: sse.Zstd, window: 19}} {
		s = append(s, z, setting{coding: z.coding, window: z.window, dense: true})
	}
	for w := 20; w >= 15; w-- {
		s = append(s, setting{coding: sse.Zstd, window: w, lowMem: true}, setting{coding: sse.Zstd, window: w, lowMem: true, dense: true})
	}
	w := 16
	for int64(n)*16 > int64(15)<<w {
		w++
	}
	return append(s, setting{coding: sse.Brotli, window: w, q: 3}, setting{coding: sse.Brotli, window: w, q: 4})
}

// TestSlowLinks measures what the denser settings a slow stream asks for
// cost and save: zstd's default level against its fastest at each tier, on
// the frames of TestFrameSizes, the page and a Game of Life, and brotli's
// quality 4 against 3. The heap at the default level gives the budget its
// tiers for slow streams. It writes slow.csv.
func TestSlowLinks(t *testing.T) {
	request(t)
	const streams = 64
	workloads := []struct {
		name   string
		frames [][]byte
	}{
		{"board 50 KB", boardRows(t, 70, 200)},
		{"board 142 KB", boardRows(t, 200, 200)},
		{"board 230 KB", boardRows(t, 330, 200)},
		{"table 230 KB", tableFrames(690, 200)},
		{"list 230 KB", listFrames(1300, 200)},
		{"page", pageFrames(150, 1)},
		{"life", lifeFrames(200)},
	}
	for _, w := range workloads {
		t.Logf("%s: %s", w.name, sizes(w.frames))
	}
	t.Logf("load average %s", loadavg())
	f, err := os.Create("slow.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := csv.NewWriter(f)
	c.Write([]string{"workload", "setting", "frame_bytes", "heap_kib_per_stream", "first_frame_bytes", "bytes_per_frame", "cpu_us_per_frame"})
	for _, w := range workloads {
		largest := 0
		for _, f := range w.frames {
			largest = max(largest, len(f))
		}
		for _, s := range slowSettings(largest) {
			r := measure(t, w.name, w.frames, s, streams)
			c.Write([]string{w.name, s.String(), strconv.FormatFloat(r.in, 'f', 0, 64), strconv.FormatFloat(r.heap/1024, 'f', 0, 64),
				strconv.Itoa(r.first), strconv.FormatFloat(r.perFrame, 'f', 0, 64), strconv.FormatFloat(float64(r.cpu[0])/1e3, 'f', 1, 64)})
			c.Flush()
			t.Logf("%-13s %-32s %6.0f KiB  first %6d B  %6.0f B/frame  %7.1f us", w.name, s, r.heap/1024, r.first, r.perFrame, float64(r.cpu[0])/1e3)
		}
	}
	t.Logf("load average %s", loadavg())
	if err := c.Error(); err != nil {
		t.Fatal(err)
	}
}
