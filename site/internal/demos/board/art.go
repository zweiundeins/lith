package board

import "github.com/zweiundeins/lith/site/internal/pixelart"

// art is the demo's glyph: a board of three lanes, a card lifted out of
// the first on its way to the last, its edge glowing.
var art = pixelart.SVG(20, 15, "", pixelart.Layer{Grid: pixelart.Sprite(map[rune]string{
	'D': "#0A0806",
	'S': "#2B2723",
	'C': "#524C47",
	'W': "#F2F0EC",
	'w': "#CBC6C1",
	'G': "#8B857F",
	'E': "var(--lith-art-seam, #F48E1F)",
	'e': "var(--lith-art-seam-edge, #C15613)",
	'o': "var(--lith-art-seam-core, #FEECCE)",
},
	"..........DDDDDDD...",
	".........DeEEEEEeD..",
	".........DEoooooED..",
	"DDDDDDDDDDEeeeeeEDD.",
	"DSSSSSDSSDDeEEEeDSD.",
	"DSWWWSDSSDDDDDDDDSD.",
	"DSwwwSDSWWWSDSSSSSD.",
	"DSSSSSDSwwwSDSWWWSD.",
	"DSWWWSDSSSSSDSwwwSD.",
	"DSwwwSDSWWWSDSSSSSD.",
	"DSSSSSDSwwwSDSWWWSD.",
	"DSGGGSDSSSSSDSwwwSD.",
	"DSSSSSDSSSSSDSSSSSD.",
	"DCCCCCDCCCCCDCCCCCD.",
	"DDDDDDDDDDDDDDDDDDD.",
)})
