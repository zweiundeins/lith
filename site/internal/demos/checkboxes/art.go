package checkboxes

import "github.com/zweiundeins/lith/site/internal/pixelart"

// art is the demo's glyph: a grid of boxes, some of them lit.
var art = pixelart.SVG(20, 15, "", pixelart.Layer{Grid: pixelart.Sprite(map[rune]string{
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
	"GGGG.eeee.GGGG.eeee.",
	"GSSG.eoEe.GSSG.eoEe.",
	"GSSG.eEEe.GSSG.eEEe.",
	"GGGG.eeee.GGGG.eeee.",
	"....................",
	"eeee.eeee.GGGG.GGGG.",
	"eoEe.eoEe.GSSG.GSSG.",
	"eEEe.eEEe.GSSG.GSSG.",
	"eeee.eeee.GGGG.GGGG.",
	"....................",
)})
