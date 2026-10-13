package guestbook

import "github.com/zweiundeins/lith/site/internal/pixelart"

// art is the demo's glyph: an open book with a quill, its tip glowing.
var art = pixelart.SVG(20, 15, "", pixelart.Layer{Grid: pixelart.Sprite(map[rune]string{
	'D': "#0A0806",
	'W': "#F2F0EC",
	'w': "#CBC6C1",
	'G': "#8B857F",
	'S': "#524C47",
	'C': "#6B655F",
	'E': "var(--lith-art-seam, #F48E1F)",
	'e': "var(--lith-art-seam-edge, #C15613)",
	'o': "var(--lith-art-seam-core, #FEECCE)",
},
	"................DD..",
	"...............DEeD.",
	"..............DEeD..",
	".DDDDDD..DDDDDEeD...",
	"DWWWWWWDDWWWWDEeDWD.",
	"DWGGGGWDDWGGDEeDGWD.",
	"DWWWWWWDDWWWDeDWWWD.",
	"DWGGGWWDDWGGDoDGGWD.",
	"DWWWWWWDDWWWWDWWWWD.",
	"DWGGGGWDDWGGGGGGWWD.",
	"DWWWWWWDDWWWWWWWWWD.",
	"DwwwwwwDDwwwwwwwwwD.",
	"DDDDDDDSSDDDDDDDDDD.",
	".CCCCCCSSCCCCCCCCC..",
	"..DDDDDDDDDDDDDDDD..",
)})
