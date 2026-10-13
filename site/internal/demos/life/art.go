package life

import "github.com/zweiundeins/lith/site/internal/pixelart"

// art is the demo's glyph: a small dark board of six by four cells, dots
// where they are dead, and a glider in ember crawling down to the right,
// the cells it left still glowing behind it.
var art = func() string {
	const w, h = 21, 15
	g := pixelart.NewGrid(w, h)
	for y := range h {
		for x := range w {
			switch {
			case (x == 0 || x == w-1) && (y == 0 || y == h-1):
			case x == 0 || y == 0 || x == w-1 || y == h-1:
				g.Set(x, y, "#524C47")
			default:
				g.Set(x, y, "#0A0806")
			}
		}
	}
	cell := func(i, j int, c, top string) {
		x, y := 2+3*i, 2+3*j
		g.Set(x, y, top)
		g.Set(x+1, y, c)
		g.Set(x, y+1, c)
		g.Set(x+1, y+1, c)
	}
	for j := range 4 {
		for i := range 6 {
			g.Set(2+3*i, 2+3*j, "#3A3530")
		}
	}
	const (
		seam  = "var(--lith-art-seam, #F48E1F)"
		edge  = "var(--lith-art-seam-edge, #C15613)"
		core  = "var(--lith-art-seam-core, #FEECCE)"
		trail = "#5A2A0C"
	)
	// The glider's last generations, cooling.
	cell(1, 0, trail, trail)
	cell(2, 1, trail, edge)
	cell(1, 2, trail, trail)
	// The glider.
	cell(3, 0, seam, core)
	cell(4, 1, seam, core)
	cell(2, 2, edge, seam)
	cell(3, 2, seam, core)
	cell(4, 2, seam, core)
	return pixelart.SVG(w, h, "", pixelart.Layer{Grid: g})
}()
