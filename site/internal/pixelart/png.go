package pixelart

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"regexp"
	"strconv"
)

// Raster images of the art, for what takes no SVG or CSS variables: the
// Open Graph image, the touch icon and favicon.ico. Variables render as
// their fallbacks.

var hexColor = regexp.MustCompile(`#([0-9A-Fa-f]{6})`)

// rgba reads "#RRGGBB", or the fallback of "var(--x, #RRGGBB)".
func rgba(c string) (color.RGBA, bool) {
	m := hexColor.FindStringSubmatch(c)
	if m == nil {
		return color.RGBA{}, false
	}
	v, _ := strconv.ParseUint(m[1], 16, 32)
	return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255}, true
}

// canvas is an image filled with bg.
func canvas(w, h int, bg string) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if c, ok := rgba(bg); ok {
		for i := 0; i < len(img.Pix); i += 4 {
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
		}
	}
	return img
}

// draw paints layers at scale, their origin at ox, oy.
func draw(img *image.RGBA, ox, oy, scale int, layers ...Layer) {
	for _, l := range layers {
		for y := 0; y < l.Grid.H; y++ {
			for x := 0; x < l.Grid.W; x++ {
				c, ok := rgba(l.Grid.At(x, y))
				if !ok {
					continue
				}
				px, py := ox+(l.X+x)*scale, oy+(l.Y+y)*scale
				for dy := 0; dy < scale; dy++ {
					for dx := 0; dx < scale; dx++ {
						img.SetRGBA(px+dx, py+dy, c)
					}
				}
			}
		}
	}
}

func encode(img image.Image) []byte {
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

// Background is the dark theme's page colour (--lith-stone-12), for the
// raster images.
const Background = "#100E0C"

// Icon is the logo on the page colour, size×size pixels.
func Icon(size int) []byte {
	stone, halo, seam := logoSlab().layers()
	scale := max(1, size*7/8/stone.H)
	img := canvas(size, size, Background)
	draw(img, (size-stone.W*scale)/2, (size-stone.H*scale)/2, scale,
		Layer{Grid: stone}, Layer{Grid: halo}, Layer{Grid: seam})
	return encode(img)
}

// SocialImage is the Open Graph image, 1200×630: the monolith beside the
// word LITH.
func SocialImage() []byte {
	const w, h = 1200, 630
	img := canvas(w, h, Background)
	_, _, layers := heroLayers()
	const scale = 9
	draw(img, 110, h-heroH*scale-6, scale, layers...)
	word := Word("LITH", seamMid)
	draw(img, 560, 150, 24, Layer{Grid: word})
	for i, line := range []string{"LINEAR WRITES", "IMMEDIATE MODE", "TICKS", "HYPERMEDIA"} {
		y := 350 + i*48
		draw(img, 564, y, 5, Layer{Grid: Word(line[:1], seamMid)})
		draw(img, 564+6*5, y, 5, Layer{Grid: Word(line[1:], stoneTop)})
	}
	return encode(img)
}
