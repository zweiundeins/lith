package town

import "github.com/zweiundeins/lith/site/internal/pixelart"

// art is the demo's glyph: a house and a tree on their ground, and under
// them a slider, its thumb glowing.
var art = pixelart.SVG(20, 15, "", pixelart.Layer{Grid: pixelart.Sprite(map[rune]string{
	'D': "#0A0806",
	'W': "#F2F0EC",
	'G': "#524C47",
	'C': "#524C47",
	'L': "#6E9A52",
	'l': "#8DB86A",
	'T': "#6B4A2F",
	'g': "#3F5E33",
	'R': "var(--lith-art-seam-edge, #C15613)",
	'r': "var(--lith-art-seam, #F48E1F)",
	'E': "var(--lith-art-seam, #F48E1F)",
	'e': "var(--lith-art-seam-edge, #C15613)",
	'o': "var(--lith-art-seam-core, #FEECCE)",
},
	"....................",
	"....DDD.............",
	"...DrrrD.......LL...",
	"..DrRRRrD.....LlLL..",
	".DrRRRRRrD...LLLLlL.",
	".DDWWWWWDD...LlLLLL.",
	"..DWGWWWD.....LLLL..",
	"..DWGWGWD......TT...",
	"..DWWWGWD......TT...",
	"gggggggggggggggggggg",
	"....................",
	".............DDD....",
	"eEEEEEEEEEEEEDoDCCCC",
	".............DDD....",
	"....................",
)})
