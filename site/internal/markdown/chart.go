package markdown

import (
	"encoding/csv"
	"errors"
	"fmt"
	"html"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Chart is a ```chart block: lines of key: value, a line with ---, and the
// data as CSV rows of series, x and y.
//
//	```chart
//	kind: bars
//	title: Commands a second, by producers
//	x: producers
//	y: commands a second
//	source: bench/RESULTS.md
//	---
//	FULL,1,526
//	NORMAL,1,13049
//	```
//
// kind is bars (each x is a group of bars, one per series) or lines (x is
// a number). source is the file the numbers come from, relative to lith's
// repository; the docs' tests check that every number appears there.
// x-prefix and x-suffix go around each x where the chart shows it (2^ for
// windows). The other keys (columns, where, series, step) tell cmd/docgen
// how to fill the rows from a CSV file, so the rows of such a chart are
// generated.
type Chart struct {
	Kind, Title, X, Y, Source string
	// Meta holds every key of the spec, those above included.
	Meta map[string]string
	Rows []ChartRow
}

// ChartRow is one number of a chart: its series, its x and its y, as
// written in the block.
type ChartRow struct{ Series, X, Y string }

// ParseChart reads the body of a ```chart block.
func ParseChart(src string) (*Chart, error) {
	head, data, ok := strings.Cut(src, "\n---\n")
	if !ok {
		head, ok = strings.CutSuffix(strings.TrimRight(src, "\n"), "\n---")
		if !ok {
			return nil, errors.New("chart: no --- line between the spec and the data")
		}
		data = ""
	}
	c := &Chart{Meta: map[string]string{}}
	for line := range strings.Lines(head) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("chart: %q is not key: value", line)
		}
		c.Meta[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	c.Kind, c.Title, c.X, c.Y, c.Source = c.Meta["kind"], c.Meta["title"], c.Meta["x"], c.Meta["y"], c.Meta["source"]
	if c.Kind != "bars" && c.Kind != "lines" {
		return nil, fmt.Errorf("chart: kind %q, want bars or lines", c.Kind)
	}
	if c.Title == "" || c.Source == "" {
		return nil, errors.New("chart: a chart needs a title and a source")
	}
	r := csv.NewReader(strings.NewReader(data))
	r.FieldsPerRecord = 3
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("chart %q: %w", c.Title, err)
	}
	for _, rec := range records {
		row := ChartRow{Series: rec[0], X: rec[1], Y: rec[2]}
		if _, err := strconv.ParseFloat(row.Y, 64); err != nil {
			return nil, fmt.Errorf("chart %q: y %q is not a number", c.Title, row.Y)
		}
		if _, err := strconv.ParseFloat(row.X, 64); c.Kind == "lines" && err != nil {
			return nil, fmt.Errorf("chart %q: x %q is not a number", c.Title, row.X)
		}
		c.Rows = append(c.Rows, row)
	}
	if len(c.Rows) == 0 {
		return nil, fmt.Errorf("chart %q: no data", c.Title)
	}
	if len(c.series()) > len(seriesClasses) {
		return nil, fmt.Errorf("chart %q: more than %d series", c.Title, len(seriesClasses))
	}
	return c, nil
}

// Spec is the block's text above its data, as written.
func Spec(src string) string {
	head, _, ok := strings.Cut(src, "\n---\n")
	if !ok {
		head, _ = strings.CutSuffix(strings.TrimRight(src, "\n"), "\n---")
	}
	return head
}

// seriesClasses colour the series in order: docs.css gives each its hue,
// validated for colour vision deficiencies in both themes.
var seriesClasses = []string{"chart__s1", "chart__s2", "chart__s3", "chart__s4"}

// series lists the series in the order they first appear.
func (c *Chart) series() []string {
	var s []string
	for _, r := range c.Rows {
		if !slices.Contains(s, r.Series) {
			s = append(s, r.Series)
		}
	}
	return s
}

// groups lists the x values in the order they first appear.
func (c *Chart) groups() []string {
	var g []string
	for _, r := range c.Rows {
		if !slices.Contains(g, r.X) {
			g = append(g, r.X)
		}
	}
	return g
}

// The SVG's coordinates. Text is set in the UI face at 12 units, about
// 7.2 units a character.
const (
	chartW    = 640.0
	charW     = 7.2
	plotTop   = 34.0
	plotH     = 210.0
	tickGap   = 8.0
	maxBarW   = 24.0
	barGap    = 2.0
	dotRadius = 4.0
)

// HTML renders the chart as a figure: a caption with its title and
// source, the SVG, a legend for two series or more, and a table of the
// numbers.
func (c *Chart) HTML() string {
	var b strings.Builder
	fmt.Fprintf(&b, `<figure class="chart chart--%s">`, c.Kind)
	b.WriteString(`<figcaption class="chart__bar"><span class="chart__title">`)
	b.WriteString(html.EscapeString(c.Title))
	fmt.Fprintf(&b, `</span><a class="chart__source" href="%s">%s</a></figcaption>`, html.EscapeString(SourceURL(c.Source)), html.EscapeString(c.Source))
	b.WriteString(`<div class="chart__plot">`)
	if c.Kind == "bars" {
		b.WriteString(c.bars())
	} else {
		b.WriteString(c.lines())
	}
	b.WriteString(`</div>`)
	series := c.series()
	if len(series) > 1 {
		b.WriteString(`<ul class="chart__legend">`)
		for i, s := range series {
			fmt.Fprintf(&b, `<li><span class="chart__key %s" aria-hidden="true"></span>%s</li>`, seriesClasses[i], html.EscapeString(s))
		}
		b.WriteString(`</ul>`)
	}
	b.WriteString(c.table())
	b.WriteString("</figure>\n")
	return b.String()
}

// value is the y of a series at an x, as written, and whether there is one.
func (c *Chart) value(series, x string) (string, bool) {
	for _, r := range c.Rows {
		if r.Series == series && r.X == x {
			return r.Y, true
		}
	}
	return "", false
}

func (c *Chart) table() string {
	var b strings.Builder
	series := c.series()
	b.WriteString(`<details class="chart__data"><summary>The numbers</summary><table><thead><tr>`)
	fmt.Fprintf(&b, `<th scope="col">%s</th>`, html.EscapeString(c.X))
	for _, s := range series {
		fmt.Fprintf(&b, `<th scope="col">%s</th>`, html.EscapeString(s))
	}
	b.WriteString(`</tr></thead><tbody>`)
	xs := c.groups()
	if c.Kind == "lines" {
		slices.SortStableFunc(xs, func(a, b string) int { return cmpNum(a, b) })
	}
	for _, x := range xs {
		fmt.Fprintf(&b, `<tr><th scope="row">%s</th>`, html.EscapeString(c.xLabel(x)))
		for _, s := range series {
			v, ok := c.value(s, x)
			if ok {
				v = Number(v)
			}
			fmt.Fprintf(&b, `<td>%s</td>`, html.EscapeString(v))
		}
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table></details>`)
	return b.String()
}

func cmpNum(a, b string) int {
	x, _ := strconv.ParseFloat(a, 64)
	y, _ := strconv.ParseFloat(b, 64)
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

// axis is a linear scale from 0 to a round maximum, with its ticks.
type axis struct {
	max   float64
	ticks []float64
	step  float64
}

// newAxis returns a scale from 0 whose ticks, four to six of them, are
// round numbers and reach at least hi.
func newAxis(hi float64) axis {
	if hi <= 0 {
		hi = 1
	}
	raw := hi / 4
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	step := 10 * mag
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if m*mag >= raw {
			step = m * mag
			break
		}
	}
	a := axis{step: step}
	for v := 0.0; ; v += step {
		a.ticks = append(a.ticks, v)
		if v >= hi-step*1e-9 {
			a.max = v
			break
		}
	}
	return a
}

// label formats a tick: thousands as K from 10,000 up, else with
// separators, and as many decimals as the step needs.
func (a axis) label(v float64) string {
	if a.max >= 10000 && v != 0 {
		return Number(strconv.FormatFloat(v/1000, 'f', decimals(a.step/1000), 64)) + "K"
	}
	return Number(strconv.FormatFloat(v, 'f', decimals(a.step), 64))
}

func decimals(step float64) int {
	for d := 0; d < 4; d++ {
		if math.Abs(step*math.Pow(10, float64(d))-math.Round(step*math.Pow(10, float64(d)))) < 1e-9 {
			return d
		}
	}
	return 4
}

// Number puts thousands separators into a number as written: 34893 is
// 34,893, 0.640 stays 0.640.
func Number(s string) string {
	sign, rest := "", s
	if strings.HasPrefix(rest, "-") {
		sign, rest = "-", rest[1:]
	}
	whole, frac, hasFrac := strings.Cut(rest, ".")
	if len(whole) < 4 || strings.Trim(whole, "0123456789") != "" {
		return s
	}
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if hasFrac {
		return sign + b.String() + "." + frac
	}
	return sign + b.String()
}

func num(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func textWidth(s string) float64 { return float64(len([]rune(s))) * charW }

// frame draws the grid, the y axis's ticks and its label, and returns the
// plot's left edge.
func (c *Chart) frame(b *strings.Builder, y axis, left, right float64) {
	bottom := plotTop + plotH
	for _, t := range y.ticks {
		py := plotTop + plotH - t/y.max*plotH
		cls := "chart__grid"
		if t == 0 {
			cls = "chart__baseline"
		}
		fmt.Fprintf(b, `<line class="%s" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, cls, left, chartW-right, py, py)
		fmt.Fprintf(b, `<text class="chart__tick" x="%.1f" y="%.1f" text-anchor="end">%s</text>`, left-tickGap, py+4, y.label(t))
	}
	fmt.Fprintf(b, `<text class="chart__axis" x="%.1f" y="%.1f">%s</text>`, left-tickGap-maxTickWidth(y), plotTop-18, html.EscapeString(c.Y))
	fmt.Fprintf(b, `<text class="chart__axis" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, left+(chartW-right-left)/2, bottom+40, html.EscapeString(c.X))
}

func maxTickWidth(y axis) float64 {
	w := 0.0
	for _, t := range y.ticks {
		w = max(w, textWidth(y.label(t)))
	}
	return w
}

func (c *Chart) open(b *strings.Builder) {
	h := plotTop + plotH + 50
	fmt.Fprintf(b, `<svg viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, chartW, h, html.EscapeString(c.Title))
}

// bars draws groups of bars, one group per x and one bar per series, with
// each bar's value on its cap. A bar is as wide as its value's label, so
// labels stay over their bars; where they cannot, a label that would touch
// its left neighbour's moves up above it.
func (c *Chart) bars() string {
	var b strings.Builder
	series, groups := c.series(), c.groups()
	hi, label := 0.0, 0.0
	for _, r := range c.Rows {
		hi = max(hi, num(r.Y))
		label = max(label, valueWidth(Number(r.Y)))
	}
	y := newAxis(hi)
	left := maxTickWidth(y) + tickGap + 4
	right := 8.0
	c.open(&b)
	c.frame(&b, y, left, right)
	groupW := (chartW - right - left) / float64(len(groups))
	n := float64(len(series))
	barW := min(max(maxBarW, label+4), (groupW*0.8-barGap*(n-1))/n)
	bottom := plotTop + plotH
	type text struct{ x, y, w float64 }
	var labels strings.Builder
	for gi, g := range groups {
		center := left + groupW*(float64(gi)+0.5)
		x0 := center - (barW*n+barGap*(n-1))/2
		var prev *text
		for si, s := range series {
			v, ok := c.value(s, g)
			if !ok {
				continue
			}
			h := num(v) / y.max * plotH
			x := x0 + float64(si)*(barW+barGap)
			fmt.Fprintf(&b, `<g class="%s"><title>%s</title>`, seriesClasses[si], html.EscapeString(c.describe(s, g, v)))
			fmt.Fprintf(&b, `<rect class="chart__mark" x="%.1f" y="%.1f" width="%.1f" height="%.1f"/></g>`, x, bottom-h, barW, max(h, 0.5))
			t := text{x: x + barW/2, y: bottom - h - 6, w: valueWidth(Number(v))}
			if prev != nil && t.x-t.w/2 < prev.x+prev.w/2+3 && math.Abs(t.y-prev.y) < 13 {
				t.y = prev.y - 13
			}
			prev = &t
			fmt.Fprintf(&labels, `<text class="chart__value" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, t.x, t.y, Number(v))
		}
		fmt.Fprintf(&b, `<text class="chart__tick" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, center, bottom+18, html.EscapeString(c.xLabel(g)))
	}
	b.WriteString(labels.String())
	b.WriteString(`</svg>`)
	return b.String()
}

// valueWidth is the width of a value's label, set at 11.5 units.
func valueWidth(s string) float64 { return float64(len([]rune(s))) * 11.5 * 0.6 }

// xLabel is an x as the chart shows it.
func (c *Chart) xLabel(x string) string { return c.Meta["x-prefix"] + Number(x) + c.Meta["x-suffix"] }

// describe is a mark's tooltip.
func (c *Chart) describe(series, x, y string) string {
	s := series + ", " + c.X + " " + c.xLabel(x) + ": " + Number(y)
	if c.Y != "" {
		s += " " + c.Y
	}
	return s
}

// lines draws a line per series over a numeric x, with a dot and the
// value at its end.
func (c *Chart) lines() string {
	var b strings.Builder
	series := c.series()
	hiY, hiX := 0.0, 0.0
	for _, r := range c.Rows {
		hiY, hiX = max(hiY, num(r.Y)), max(hiX, num(r.X))
	}
	y, x := newAxis(hiY), newAxis(hiX)
	left := maxTickWidth(y) + tickGap + 4
	right := 16.0
	ends := make([]string, len(series))
	for i, s := range series {
		ends[i] = Number(c.last(s).Y)
		right = max(right, textWidth(ends[i])+18)
	}
	c.open(&b)
	c.frame(&b, y, left, right)
	plotW := chartW - right - left
	px := func(v float64) float64 { return left + v/x.max*plotW }
	py := func(v float64) float64 { return plotTop + plotH - v/y.max*plotH }
	bottom := plotTop + plotH
	for _, t := range x.ticks {
		fmt.Fprintf(&b, `<text class="chart__tick" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, px(t), bottom+18, x.label(t))
	}
	labels := make([]float64, len(series))
	for i, s := range series {
		labels[i] = py(num(c.last(s).Y)) + 4
	}
	spread(labels, 13)
	for i, s := range series {
		var pts []ChartRow
		for _, r := range c.Rows {
			if r.Series == s {
				pts = append(pts, r)
			}
		}
		slices.SortStableFunc(pts, func(a, b ChartRow) int { return cmpNum(a.X, b.X) })
		fmt.Fprintf(&b, `<g class="%s">`, seriesClasses[i])
		b.WriteString(`<polyline class="chart__line" points="`)
		for j, p := range pts {
			if j > 0 {
				b.WriteByte(' ')
			}
			fmt.Fprintf(&b, "%.1f,%.1f", px(num(p.X)), py(num(p.Y)))
		}
		b.WriteString(`"/>`)
		end := pts[len(pts)-1]
		fmt.Fprintf(&b, `<circle class="chart__dot" cx="%.1f" cy="%.1f" r="%.0f"/>`, px(num(end.X)), py(num(end.Y)), dotRadius)
		fmt.Fprintf(&b, `<text class="chart__value" x="%.1f" y="%.1f">%s</text>`, px(num(end.X))+10, labels[i], ends[i])
		for _, p := range pts {
			fmt.Fprintf(&b, `<circle class="chart__hit" cx="%.1f" cy="%.1f" r="7"><title>%s</title></circle>`, px(num(p.X)), py(num(p.Y)), html.EscapeString(c.describe(s, p.X, p.Y)))
		}
		b.WriteString(`</g>`)
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// last is a series' point with the largest x.
func (c *Chart) last(series string) ChartRow {
	var end ChartRow
	found := false
	for _, r := range c.Rows {
		if r.Series == series && (!found || num(r.X) >= num(end.X)) {
			end, found = r, true
		}
	}
	return end
}

// spread moves the end labels' baselines apart until each is at least gap
// from the next, keeping their order.
func spread(ys []float64, gap float64) {
	idx := make([]int, len(ys))
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(a, b int) int { return cmpFloat(ys[a], ys[b]) })
	for k := 1; k < len(idx); k++ {
		if d := ys[idx[k]] - ys[idx[k-1]]; d < gap {
			ys[idx[k]] += gap - d
		}
	}
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
