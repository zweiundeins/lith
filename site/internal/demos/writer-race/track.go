package writerrace

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/zweiundeins/lith/sqlite"
)

// track is the race on the track as the page draws it: the race running
// now, or the last one.
type track struct {
	Race     Race // ID 0: no race yet
	Running  bool
	Rate     float64 // commands a second: the last quarter second while it runs, the whole race once it is over
	PerTx    float64 // commands per transaction, likewise
	Progress float64 // of Duration, 0 to 1
	Chart    chart
	Bars     []bar
	P99s     string // the commit p99 of every sample, in ms: JSON for sb-sparkline
	Fresh    bool   // the race ended a moment ago
}

// window is how many samples the numbers of a running race cover: a
// quarter second.
const window = 5

func loadTrack(ctx context.Context, q sqlite.Querier) (track, error) {
	r, err := onTrack(ctx, q)
	if err != nil || r.ID == 0 {
		return track{}, err
	}
	pts, err := points(ctx, q, r.ID)
	if err != nil {
		return track{}, err
	}
	t := newTrack(r, pts)
	t.Fresh = r.Over() && time.Since(r.EndedAt) < fresh
	return t, nil
}

func newTrack(r Race, pts []Point) track {
	t := track{Race: r, Running: r.State == "running", Rate: r.Rate(), PerTx: r.PerTx()}
	t.Progress = min(1, float64(r.Last.At)/float64(Duration))
	if t.Running && len(pts) > 0 {
		last, first := pts[len(pts)-1], Point{} // the race's start, until a window has passed
		if len(pts) > window {
			first = pts[len(pts)-1-window]
		}
		if dt := last.At - first.At; dt > 0 {
			t.Rate = float64(last.Commands-first.Commands) / dt.Seconds()
		}
		if db := last.Batches - first.Batches; db > 0 {
			t.PerTx = float64(last.Commands-first.Commands) / float64(db)
		}
	}
	t.Chart = newChart(rates(pts), max(Duration, r.Last.At), float64(bench[r.Settings]))
	t.Bars = bars(r.Last.Sizes)
	t.P99s = series(pts)
	return t
}

// step is the rate of commands over the time from the sample before.
type step struct {
	At   time.Duration
	Rate float64
}

// rates turns cumulative samples into commands a second between them. A
// sample less than half an interval after the one before joins the next
// interval, and the last sample of a race (taken when its producers
// stopped, often a moment after the one before) widens the last interval
// instead of making a spike of its own.
func rates(pts []Point) []step {
	type mark struct {
		at time.Duration
		n  int64
	}
	marks := []mark{{}}
	for i, p := range pts {
		dt := p.At - marks[len(marks)-1].at
		switch {
		case dt <= 0:
			continue
		case dt < Every/2 && i < len(pts)-1:
			continue
		case dt < Every/2 && len(marks) > 1:
			marks[len(marks)-1] = mark{p.At, p.Commands}
			continue
		}
		marks = append(marks, mark{p.At, p.Commands})
	}
	out := make([]step, 0, len(marks)-1)
	for i := 1; i < len(marks); i++ {
		a, b := marks[i-1], marks[i]
		out = append(out, step{At: b.at, Rate: float64(b.n-a.n) / (b.at - a.at).Seconds()})
	}
	return out
}

// The chart's viewBox: the line steps by its whole units, and the page
// stretches it to the chart's box, strokes at their own width.
const (
	chartW = 300
	chartH = 80
)

// chart is the race's commands a second over time, as SVG path data.
type chart struct {
	Line, Area string
	Head       string // the newest point's place in the box, as CSS
	Top        string // the y axis' top, "100k"
	Bench      string // the benchmark's number, "66,688"; "" for none
	BenchY     int
	BenchAt    string // the benchmark line's place from the top, a CSS percentage
	Ticks      []tick // the x axis
}

type tick struct {
	Label string
	Left  string // CSS percentage
}

func newChart(steps []step, span time.Duration, ref float64) chart {
	var c chart
	top := ref
	for _, s := range steps {
		top = max(top, s.Rate)
	}
	top = nice(top * 1.08)
	c.Top = short(top)
	y := func(v float64) int {
		return chartH - int(math.Round(min(v, top)/top*chartH))
	}
	x := func(at time.Duration) int {
		return int(math.Round(float64(min(at, span)) / float64(span) * chartW))
	}
	if ref > 0 {
		c.Bench, c.BenchY = group(int64(ref)), y(ref)
		c.BenchAt = pct(float64(c.BenchY) / chartH)
	}
	for s := time.Duration(0); s <= span; s += time.Second {
		c.Ticks = append(c.Ticks, tick{Label: fmt.Sprintf("%d s", s/time.Second), Left: pct(float64(s) / float64(span))})
	}
	if len(steps) == 0 {
		return c
	}
	var line, area strings.Builder
	y0 := y(steps[0].Rate)
	fmt.Fprintf(&line, "M0 %d", y0)
	fmt.Fprintf(&area, "M0 %dV%d", chartH, y0)
	var hx, hy int
	for i, s := range steps {
		hx, hy = x(s.At), y(s.Rate)
		if i > 0 {
			fmt.Fprintf(&line, "V%d", hy)
			fmt.Fprintf(&area, "V%d", hy)
		}
		fmt.Fprintf(&line, "H%d", hx)
		fmt.Fprintf(&area, "H%d", hx)
	}
	fmt.Fprintf(&area, "V%dZ", chartH)
	c.Line, c.Area = line.String(), area.String()
	c.Head = "inset-inline-start: " + pct(float64(hx)/chartW) + "; inset-block-start: " + pct(float64(hy)/chartH)
	return c
}

// series writes the samples' commit p99 in milliseconds as a JSON array.
func series(pts []Point) string {
	b := []byte{'['}
	for i, p := range pts {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendFloat(b, float64(p.P99)/float64(time.Millisecond), 'f', 2, 64)
	}
	return string(append(b, ']'))
}

// nice rounds v up to a number that labels an axis well: 1, 1.2, 1.5, 2,
// 2.5, 3, 4, 5, 6 or 8 times a power of ten.
func nice(v float64) float64 {
	if v <= 0 {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8} {
		if v <= m*p*(1+1e-9) {
			return m * p
		}
	}
	return 10 * p
}

// short writes a round number of the axis: 800, 2.5k, 100k.
func short(v float64) string {
	if v >= 1000 {
		return strconv.FormatFloat(v/1000, 'f', -1, 64) + "k"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func pct(f float64) string { return strconv.FormatFloat(f*100, 'f', 2, 64) + "%" }

// bar is one bucket of the histogram of transaction sizes.
type bar struct {
	Label  string // the smallest size in the bucket
	N      int64
	Height string // of the tallest, 0 to 1
}

var barLabels = [len(Sizes{})]string{"1", "2", "4", "8", "16", "32", "64", "128"}

func bars(s Sizes) []bar {
	var top int64
	for _, n := range s {
		top = max(top, n)
	}
	out := make([]bar, len(s))
	for i, n := range s {
		h := 0.0
		if top > 0 {
			h = float64(n) / float64(top)
		}
		out[i] = bar{Label: barLabels[i], N: n, Height: strconv.FormatFloat(h, 'f', 3, 64)}
	}
	return out
}

// group writes n with a comma between thousands: 66,688.
func group(n int64) string {
	s := strconv.FormatInt(n, 10)
	if n < 0 {
		return "-" + group(-n)
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// ms writes a duration in milliseconds, with one decimal below ten.
func ms(d time.Duration) string {
	v := float64(d) / float64(time.Millisecond)
	if v < 10 {
		return strconv.FormatFloat(v, 'f', 1, 64) + " ms"
	}
	return strconv.FormatFloat(v, 'f', 0, 64) + " ms"
}
