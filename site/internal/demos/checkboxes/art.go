package checkboxes

import "github.com/zweiundeins/lith/site/internal/pixelart"

// art is the demo's glyph: a grid of boxes, some of them lit, and a
// pointer that has just lit one more.
var art = pixelart.SVG(20, 15, "",
	pixelart.Layer{Grid: pixelart.Sprite(map[rune]string{
		'G': "#8B857F",
		'S': "#524C47",
		'E': "var(--lith-art-seam, #F48E1F)",
		'e': "var(--lith-art-seam-edge, #C15613)",
		'o': "var(--lith-art-seam-core, #FEECCE)",
	},
		"eeee.GGGG.eeee.GGGG.",
		"eoEe.GSSG.eoEe.GSSG.",
		"eEEe.GSSG.eEEe.GSSG.",
		"eeee.GGGG.eeee.GGGG.",
		"....................",
		"GGGG.eeee.eeee.eeee.",
		"GSSG.eoEe.eoEe.eoEe.",
		"GSSG.eEEe.eEEe.eEEe.",
		"GGGG.eeee.eeee.eeee.",
		"....................",
		"eeee.eeee.GGGG.GGGG.",
		"eoEe.eoEe.GSSG.GSSG.",
		"eEEe.eEEe.GSSG.GSSG.",
		"eeee.eeee.GGGG.GGGG.",
	)},
	pixelart.Layer{X: 12, Y: 7, Grid: pixelart.Sprite(map[rune]string{
		'D': "#0A0806",
		'W': "#F2F0EC",
	},
		"D.....",
		"DD....",
		"DWD...",
		"DWWD..",
		"DWWWD.",
		"DWDDDD",
		"DD....",
	)},
)
