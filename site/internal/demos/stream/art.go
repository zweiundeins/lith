package stream

import "github.com/zweiundeins/lith/site/internal/pixelart"

// art is the demo's glyph: a small screen that plots bytes per frame, the
// first frame tall, the ones after it low, and the stream running on.
var art = pixelart.SVG(20, 15, "", pixelart.Layer{Grid: pixelart.Sprite(map[rune]string{
	'D': "#0A0806",
	'S': "#524C47",
	'C': "#6B655F",
	'G': "#8B857F",
	'w': "#CBC6C1",
	'k': "#1A1714",
	'g': "#2A2622",
	'E': "var(--lith-art-seam, #F48E1F)",
	'e': "var(--lith-art-seam-edge, #C15613)",
	'o': "var(--lith-art-seam-core, #FEECCE)",
},
	"DDDDDDDDDDDDDDDDDDDD",
	"DGwwwwwwwwwwwwwwwwGD",
	"DwkkkkkkkkkkkkkkkkCD",
	"DwkookkkkkkkkkkkkkCD",
	"DwkEekkgkkkgkkkgkkCD",
	"DwkEekkkkkkkkkkkkkCD",
	"DwkEekkkkkkkkkkkkkCD",
	"DwkEekkgkkkooEkgkkCD",
	"DwkEekkkkkkEekkkkkCD",
	"DwkEeEokkkkEekkoEECD",
	"DwkEeEeEEoEEeEEEekCD",
	"DCCCCCCCCCCCCCCCCCSD",
	"DDDDDDDDSSSSDDDDDDDD",
	"......DSCCCCSD......",
	".....DDDDDDDDDD.....",
)})
