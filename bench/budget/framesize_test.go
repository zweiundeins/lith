package budget

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/zweiundeins/lith/sse"
)

// tableFrames is a data table of rows rows, as sb-data-table renders a
// window of a large catalog: each frame changes a few cells, and every
// tenth frame scrolls the window by a few rows.
func tableFrames(rows, n int) [][]byte {
	rnd := rand.New(rand.NewPCG(7, 11))
	type star struct {
		id               int
		name, class, con string
		mag, ra, dec, pm float64
		dist             int
	}
	classes := []string{"O5V", "B2III", "A0V", "F5IV", "G2V", "K1III", "M3V"}
	cons := []string{"Andromeda", "Cassiopeia", "Cygnus", "Lyra", "Orion", "Perseus", "Ursa Major"}
	mk := func(i int) star {
		r := rand.New(rand.NewPCG(uint64(i), 3))
		return star{id: i, name: fmt.Sprintf("HD %d", 1000+i*7), class: classes[r.IntN(len(classes))], con: cons[r.IntN(len(cons))],
			mag: r.Float64()*12 - 1, ra: r.Float64() * 360, dec: r.Float64()*180 - 90, pm: r.Float64() * 400, dist: 4 + r.IntN(4000)}
	}
	first := 0
	win := make([]star, rows)
	for i := range win {
		win[i] = mk(i)
	}
	frames := make([][]byte, n)
	for f := range frames {
		if f > 0 {
			if f%10 == 0 {
				k := 1 + rnd.IntN(5)
				first += k
				win = append(win[k:], make([]star, k)...)
				for i := rows - k; i < rows; i++ {
					win[i] = mk(first + i)
				}
			}
			for range 3 {
				win[rnd.IntN(rows)].mag += rnd.Float64() - 0.5
			}
		}
		var b bytes.Buffer
		b.WriteString(`<sb-data-table id="stars" name="stars" sort="mag" data-on:sb-sort="@get('/demo/data/rows')"><table role="grid" aria-rowcount="100000"><thead><tr><th>Name</th><th>Class</th><th>Constellation</th><th>Mag</th><th>RA</th><th>Dec</th><th>PM</th><th>Distance</th></tr></thead><tbody>` + "\n")
		for i, s := range win {
			fmt.Fprintf(&b, `<tr id="star-%d" aria-rowindex="%d" data-key="%d"><td class="name"><a href="/stars/%d">%s</a></td><td><span class="cls cls--%c">%s</span></td><td>%s</td><td class="num">%.2f</td><td class="num">%.4f</td><td class="num">%+.4f</td><td class="num">%.1f mas/yr</td><td class="num">%d ly</td></tr>`+"\n",
				s.id, first+i+2, s.id, s.id, s.name, s.class[0], s.class, s.con, s.mag, s.ra, s.dec, s.pm, s.dist)
		}
		b.WriteString(`</tbody></table></sb-data-table>`)
		frames[f] = sse.PatchElements(b.Bytes())
	}
	return frames
}

// listFrames is a list of rows rows that change in place: before each
// frame, every hundredth row gets a new value.
func listFrames(rows, n int) [][]byte {
	frames := make([][]byte, n)
	for i := range frames {
		var b bytes.Buffer
		b.WriteString(`<main id="app"><table class="board">` + "\n")
		for r := range rows {
			v := r * 7
			if r%100 == i%100 {
				v += i
			}
			fmt.Fprintf(&b, `<tr id="row-%d" class="row"><td class="name">Item %d</td><td class="value">%d</td><td><button data-on:click="@post('/cmd/inc?id=%d')">+</button></td></tr>`+"\n", r, r, v, r)
		}
		b.WriteString("</table></main>")
		frames[i] = sse.PatchElements(b.Bytes())
	}
	return frames
}

// budgetSettings are the tiers on the budget's menu.
func budgetSettings() []setting {
	s := []setting{
		{coding: sse.Zstd, window: 20}, {coding: sse.Zstd, window: 19},
	}
	for w := 20; w >= 15; w-- {
		s = append(s, setting{coding: sse.Zstd, window: w, lowMem: true})
	}
	for w := 20; w >= 16; w-- {
		s = append(s, setting{coding: sse.Brotli, window: w, q: 4})
	}
	for w := 20; w >= 16; w-- {
		s = append(s, setting{coding: sse.Brotli, window: w, q: 3})
	}
	return append(s, setting{coding: sse.Gzip})
}

// TestFrameSizes measures the budget's tiers on frames of about 50, 142
// and 230 KB, and brotli's also as they were before a stream flushed its
// brotli encoder within a frame. It writes framesize.csv.
func TestFrameSizes(t *testing.T) {
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
	}
	for _, w := range workloads {
		t.Logf("%s: %s", w.name, sizes(w.frames))
	}
	f, err := os.Create("framesize.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := csv.NewWriter(f)
	c.Write([]string{"workload", "setting", "frame_bytes", "heap_kib_per_stream", "first_frame_bytes", "bytes_per_frame", "cpu_us_per_frame"})
	row := func(w string, name string, r result) {
		c.Write([]string{w, name, strconv.FormatFloat(r.in, 'f', 0, 64), strconv.FormatFloat(r.heap/1024, 'f', 0, 64),
			strconv.Itoa(r.first), strconv.FormatFloat(r.perFrame, 'f', 0, 64), strconv.FormatFloat(float64(r.cpu[0])/1e3, 'f', 1, 64)})
		c.Flush()
		t.Logf("%-13s %-32s %6.0f KiB  first %6d B  %6.0f B/frame  %7.1f us", w, name, r.heap/1024, r.first, r.perFrame, float64(r.cpu[0])/1e3)
	}
	for _, w := range workloads {
		for _, s := range budgetSettings() {
			row(w.name, s.String(), measure(t, w.name, w.frames, s, streams))
		}
		for _, s := range budgetSettings() {
			if s.coding == sse.Brotli && s.window <= 19 {
				row(w.name, s.String()+" unflushed", measureUnflushed(w.frames, s, streams))
			}
		}
	}
	if err := c.Error(); err != nil {
		t.Fatal(err)
	}
}

// measureUnflushed is measure for brotli encoders that take each frame
// whole and flush once, as streams did before they flushed within a frame.
func measureUnflushed(frames [][]byte, s setting, streams int) result {
	before := liveHeap()
	bufs := make([]*bytes.Buffer, streams)
	encs := make([]*brotli.Writer, streams)
	for i := range encs {
		bufs[i] = &bytes.Buffer{}
		encs[i] = brotli.NewWriterOptions(bufs[i], brotli.WriterOptions{Quality: s.q, LGWin: s.window})
	}
	var sizes []int
	cpu0, start := cpuTime(), time.Now()
	for _, f := range frames {
		for i, e := range encs {
			bufs[i].Reset()
			e.Write(f)
			e.Flush()
		}
		sizes = append(sizes, bufs[0].Len())
	}
	wall, cpu := time.Since(start), cpuTime()-cpu0
	n := len(frames) * streams
	r := result{setting: s, streams: streams, frames: len(frames), heap: float64(liveHeap()-before) / float64(streams),
		cpu: []time.Duration{cpu / time.Duration(n)}, wall: []time.Duration{wall / time.Duration(n)}, first: sizes[0]}
	rest, in := 0, 0
	for _, n := range sizes[1:] {
		rest += n
	}
	for _, f := range frames {
		in += len(f)
	}
	r.perFrame = float64(rest) / float64(len(sizes)-1)
	r.in = float64(in) / float64(len(frames))
	for _, e := range encs {
		e.Close()
	}
	return r
}
