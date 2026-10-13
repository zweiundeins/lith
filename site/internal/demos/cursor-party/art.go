package cursorparty

import "github.com/zweiundeins/lith/site/internal/pixelart"

// art is the demo's glyph: a white pointer, and an ember one arriving
// along its trail, its name tag under it.
var art = pixelart.SVG(20, 15, "", pixelart.Layer{Grid: pixelart.Sprite(map[rune]string{
	'O': "#0A0806",
	'W': "#F2F0EC",
	'w': "#CBC6C1",
	'E': "var(--lith-art-seam, #F48E1F)",
	'e': "var(--lith-art-seam-edge, #C15613)",
	'o': "var(--lith-art-seam-core, #FEECCE)",
},
	"....................",
	".O..................",
	".OO.................",
	".OWO......e.........",
	".OWWO......e........",
	".OWWWO......E.......",
	".OWWWWO......O......",
	".OWWWwwO.....OO.....",
	".OWwOOOO.....OoO....",
	".OwO.........OEEO...",
	".OO..........OEEEO..",
	".O...........OEOOO..",
	".............OO.....",
	".............O..EEEE",
	"................EEEE",
)})
