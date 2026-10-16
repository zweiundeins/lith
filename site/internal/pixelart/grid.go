// Package pixelart draws the site's 8-bit art: the monolith of the logo and
// the hero, the demos' glyphs and the icons. Art is a grid of colours,
// written as sprites (rows of palette letters) or generated, and emitted as
// SVG with one path per colour and crisp edges, so it stays sharp at any
// size. Starbase's internal/pixelart is where this began.
//
// Colours that follow the theme are CSS variables with a fallback,
// var(--lith-art-*, #hex): they take effect where the SVG is inlined into
// a page; as a file (the favicon) and in the PNGs the fallbacks show.
package pixelart

import (
	"fmt"
	"math"
	"strings"
)

// Grid is a raster of CSS colours; "" is transparent.
type Grid struct {
	W, H int
	Px   []string
}

// NewGrid returns a transparent grid.
func NewGrid(w, h int) Grid { return Grid{W: w, H: h, Px: make([]string, w*h)} }

// At is the colour at x, y; "" outside the grid.
func (g Grid) At(x, y int) string {
	if x < 0 || y < 0 || x >= g.W || y >= g.H {
		return ""
	}
	return g.Px[y*g.W+x]
}

// Set paints x, y; outside the grid it does nothing.
func (g Grid) Set(x, y int, c string) {
	if x < 0 || y < 0 || x >= g.W || y >= g.H {
		return
	}
	g.Px[y*g.W+x] = c
}

// Sprite parses rows of palette letters; '.' and ' ' are transparent.
func Sprite(palette map[rune]string, rows ...string) Grid {
	w := 0
	for _, r := range rows {
		w = max(w, len([]rune(r)))
	}
	g := NewGrid(w, len(rows))
	for y, r := range rows {
		for x, ch := range []rune(r) {
			if c, ok := palette[ch]; ok {
				g.Set(x, y, c)
			}
		}
	}
	return g
}

// Layer places a grid on the canvas, in a group with a class if it has one
// (for CSS: the seam's glow, a twinkle).
type Layer struct {
	Grid  Grid
	X, Y  int
	Class string
}

// SVG renders layers on a w×h canvas as a standalone SVG document, with css
// in a <style> element (it applies where the SVG is a file, too).
func SVG(w, h int, css string, layers ...Layer) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" class="px-art" viewBox="0 0 %d %d" width="%d" height="%d" shape-rendering="crispEdges">`, w, h, w*4, h*4)
	if css != "" {
		b.WriteString("<style>" + css + "</style>")
	}
	for _, l := range layers {
		if l.Class != "" {
			fmt.Fprintf(&b, `<g class="%s">`, l.Class)
		}
		writeRuns(&b, l.Grid, l.X, l.Y)
		if l.Class != "" {
			b.WriteString("</g>")
		}
	}
	b.WriteString("</svg>")
	return b.String()
}

// Paths is g's pixels as SVG paths, one per colour, without an <svg>
// around them: the body of a <symbol> that a page draws many times.
func Paths(g Grid) string {
	var b strings.Builder
	writeRuns(&b, g, 0, 0)
	return b.String()
}

// writeRuns writes the horizontal runs of equal colour, one path per
// colour: the same pixels as a rect each, in a fraction of the markup.
func writeRuns(b *strings.Builder, g Grid, ox, oy int) {
	type run struct{ x, y, w int }
	byColor := map[string][]run{}
	var order []string
	for y := 0; y < g.H; y++ {
		for x := 0; x < g.W; {
			c := g.At(x, y)
			if c == "" {
				x++
				continue
			}
			start := x
			for x < g.W && g.At(x, y) == c {
				x++
			}
			if _, seen := byColor[c]; !seen {
				order = append(order, c)
			}
			byColor[c] = append(byColor[c], run{start, y, x - start})
		}
	}
	for _, c := range order {
		if strings.HasPrefix(c, "var(") {
			fmt.Fprintf(b, `<path style="fill:%s" d="`, c)
		} else {
			fmt.Fprintf(b, `<path fill="%s" d="`, c)
		}
		for _, r := range byColor[c] {
			fmt.Fprintf(b, "M%d %dh%dv1h-%dz", r.x+ox, r.y+oy, r.w, r.w)
		}
		b.WriteString(`"/>`)
	}
}

// noise is deterministic 2D value noise in [0, 1).
func noise(x, y float64, seed uint32) float64 {
	xi, yi := math.Floor(x), math.Floor(y)
	xf, yf := x-xi, y-yi
	h := func(i, j float64) float64 {
		n := uint32(int32(i))*374761393 + uint32(int32(j))*668265263 + seed*2246822519
		n = (n ^ (n >> 13)) * 1274126177
		return float64(n^(n>>16)) / float64(1<<32)
	}
	s := func(t float64) float64 { return t * t * (3 - 2*t) }
	a, b := h(xi, yi), h(xi+1, yi)
	c, d := h(xi, yi+1), h(xi+1, yi+1)
	u, v := s(xf), s(yf)
	return a + (b-a)*u + (c-a)*v + (a-b-c+d)*u*v
}

// hash is a deterministic number in [0, 1) for a pixel.
func hash(x, y int, seed uint32) float64 {
	n := uint32(int32(x))*374761393 + uint32(int32(y))*668265263 + seed*2246822519
	n = (n ^ (n >> 13)) * 1274126177
	return float64(n^(n>>16)) / float64(1<<32)
}
