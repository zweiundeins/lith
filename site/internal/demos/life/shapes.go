package life

import (
	"fmt"
	"strings"

	"github.com/a-h/templ"
)

// A tool is what a tap on the board does: draw cells, or drop a pattern.
type tool struct {
	Name, Title, About string
	Shape              Pattern
}

// tools are the toolbar's, in its order: drawing, then the patterns.
var tools = func() []tool {
	ts := []tool{{Name: "draw", Title: "Draw", About: "Draw cells by dragging over the board; a stroke that starts on a live cell clears instead.", Shape: pattern("draw", "Draw", "", "O")}}
	for _, p := range Patterns {
		ts = append(ts, tool{Name: p.Name, Title: p.Title, About: p.About, Shape: p})
	}
	return ts
}()

// path is the SVG path of a pattern's live cells, a square each, in its
// box; transposed swaps the axes, for the board on its side.
func path(p Pattern, transposed bool) string {
	var b strings.Builder
	for y := range p.H {
		for x := range p.W {
			if !p.Alive(x, y) {
				continue
			}
			if transposed {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", y, x)
			} else {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x, y)
			}
		}
	}
	return b.String()
}

// shapeStyle sizes a shape's outline in cells, and names its anchor.
func shapeStyle(p Pattern) string {
	ax, ay := p.Anchor()
	return fmt.Sprintf("--w: %d; --h: %d; --ax: %d; --ay: %d", p.W, p.H, ax, ay)
}

// icon is a pattern drawn small for its button, in the button's colour.
func icon(p Pattern) templ.Component {
	return templ.Raw(fmt.Sprintf(`<svg class="life__icon" viewBox="-0.5 -0.5 %d %d" style="--w: %d; --h: %d" fill="currentColor" shape-rendering="crispEdges" aria-hidden="true" focusable="false"><path d="%s"/></svg>`,
		p.W+1, p.H+1, p.W+1, p.H+1, path(p, false)))
}

// pencil is the draw tool's icon: a stroke of cells.
var pencil = templ.Raw(`<svg class="life__icon" viewBox="0 0 8 8" style="--w: 8; --h: 8" fill="currentColor" shape-rendering="crispEdges" aria-hidden="true" focusable="false"><path d="M6 0h2v2H6zM5 2h2v1H5zM4 3h2v1H4zM3 4h2v1H3zM2 5h2v1H2zM0 6h3v2H0z"/></svg>`)

// gunIcon is the glider gun drawn small: the gun's 36 cells would blur
// into a smudge at the size of a button's icon, so it shows a block, the
// gun's curve and the glider it fires.
var gunIcon = pattern("gun-icon", "", "",
	"OO...OO......",
	"OO..O..O.....",
	"....O......O.",
	"....O..O....O",
	".....OO...OOO",
)

// toolIcon is a tool's icon.
func toolIcon(t tool) templ.Component {
	switch t.Name {
	case "draw":
		return pencil
	case "gun":
		return icon(gunIcon)
	}
	return icon(t.Shape)
}

// box is a shape's viewBox: its box, or on its side.
func box(p Pattern, transposed bool) string {
	if transposed {
		return fmt.Sprintf("0 0 %d %d", p.H, p.W)
	}
	return fmt.Sprintf("0 0 %d %d", p.W, p.H)
}
