package writerrace

import "github.com/zweiundeins/lith/site/internal/pixelart"

// art is the demo's glyph: commands like comets, one entering the
// monolith's seam while four wait their turn.
var art = pixelart.SVG(20, 15, "", pixelart.Layer{Grid: pixelart.Sprite(map[rune]string{
	'D': "#0A0806",
	'T': "#A9A39D",
	'R': "#8B857F",
	'L': "#6B655F",
	'F': "#524C47",
	'd': "#433D39",
	's': "#2B2723",
	'G': "#8B857F",
	'E': "var(--lith-art-seam, #F48E1F)",
	'e': "var(--lith-art-seam-edge, #C15613)",
	'o': "var(--lith-art-seam-core, #FEECCE)",
},
	"....................",
	"............DDDDDDD.",
	"............DTTTTRD.",
	"....LGGeeE..DLFFFsD.",
	"............DLFdFsD.",
	".LLLLLGGeeE.DLFeFsD.",
	"............DLeEFsD.",
	"LLLLLGGeEEooEooEesD.",
	"............DLeEFsD.",
	"..LLLLGGeeE.DLFeFsD.",
	"............DLFEFsD.",
	".....GGeeE..DLFeFsD.",
	"............DLFdFsD.",
	"............DLFFFsD.",
	"............DDDDDDD.",
)})
