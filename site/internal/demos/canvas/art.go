package canvas

import "github.com/zweiundeins/lith/site/internal/pixelart"

// art is the demo's glyph: a canvas of dark stone with a heart painted on
// it in the seam's colours, a few pixels other painters left, and a brush
// whose tip glows like the seam.
var art = pixelart.SVG(20, 15, "", pixelart.Layer{Grid: pixelart.Sprite(map[rune]string{
	'D': "#0A0806",
	'w': "#CBC6C1",
	'G': "#8B857F",
	'C': "#2B2723",
	'S': "#201C19",
	'E': "var(--lith-art-seam, #F48E1F)",
	'e': "var(--lith-art-seam-edge, #C15613)",
	'o': "var(--lith-art-seam-core, #FEECCE)",
},
	"..................GD",
	"DDDDDDDDDDDDDDD..GD.",
	"DCCCCCCCCCCCCCD.GD..",
	"DCeSSSSSSSSSSSDGD...",
	"DCSSEESSSEESwSDwD...",
	"DCSEoEESEEEESeED....",
	"DCSEoEEEEEEESoe.....",
	"DCSEEEEEEEEeSSD.....",
	"DCSSEEEEEEeSSSD.....",
	"DCSSSEEEEeSSSSD.....",
	"DCSSSSEEeSSSSSD.....",
	"DCSSSSSeSSSSoSD.....",
	"DCSGSSSSSSSSSSD.....",
	"DCSSSSSSSSSSSSD.....",
	"DDDDDDDDDDDDDDD.....",
)})
