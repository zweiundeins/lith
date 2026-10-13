package pixelart

// The monolith's palette: warm stone and the seam's glow. The stone keeps
// its colours in both themes, as an object would; the seam's colours are
// variables, so the stylesheet can turn its glow up and down.
const (
	outline   = "#0A0806"
	stoneTop  = "#A9A39D"
	stoneRim  = "#8B857F"
	stoneLit  = "#6B655F"
	stoneFace = "#524C47"
	stoneDim  = "#433D39"
	stoneSide = "#2B2723"
	stoneDeep = "#201C19"
	warmFar   = "#5A3F2C"
	warmNear  = "#7A4A26"
	seamEdge  = "var(--lith-art-seam-edge, #C15613)"
	seamMid   = "var(--lith-art-seam, #F48E1F)"
	seamCore  = "var(--lith-art-seam-core, #FEECCE)"
)

// slab is the shape of a monolith: a front face, a side face to its right
// and a top face, drawn inside a one-pixel outline.
type slab struct {
	W, H int // with the outline
	Side int // width of the side face
	Top  int // height of the top face
	Seed uint32
	// Branches adds short cracks off the seam, for the large monolith.
	Branches bool
}

// layers draws the slab: the stone, the warm halo around the seam (its
// own group, so CSS can dim it), and the seam.
func (s slab) layers() (stone, halo, seam Grid) {
	stone, halo, seam = NewGrid(s.W, s.H), NewGrid(s.W, s.H), NewGrid(s.W, s.H)
	front := s.W - 2 - s.Side // last column of the front face
	for y := 0; y < s.H; y++ {
		for x := 0; x < s.W; x++ {
			c := ""
			switch {
			case x == 0 || y == 0 || x == s.W-1 || y == s.H-1:
				c = outline
			case y <= s.Top && x > front:
				c = stoneRim // the side face's top
			case y <= s.Top:
				c = stoneTop
				if y == s.Top && hash(x, y, s.Seed) > 0.7 {
					c = stoneRim
				}
			case x > front:
				c = stoneSide
				if x == s.W-2 || noise(float64(x)/1.5, float64(y)/4, s.Seed+3) > 0.7 {
					c = stoneDeep
				}
			case x == 1:
				c = stoneLit // the lit left edge
			default:
				c = stoneFace
				depth := float64(y-s.Top) / float64(s.H-s.Top) // 0 at the top, 1 at the foot
				n := noise(float64(x)/2.2, float64(y)/3.4, s.Seed)
				switch {
				case n > 0.74-0.18*depth:
					c = stoneDim
				case y == s.Top+1 || hash(x, y, s.Seed+1) > 0.96:
					c = stoneLit
				}
			}
			stone.Set(x, y, c)
		}
	}
	// The seam runs from under the top face to the foot, wandering by a
	// pixel now and then, and never onto the edges.
	x := 2 + (front-2)/2
	if hash(1, 1, s.Seed) > 0.5 {
		x--
	}
	prev, still := x, 0
	for y := s.Top + 1; y < s.H-1; y++ {
		if r := hash(x, y, s.Seed+7); still >= 2 && r < 0.22 && x > 3 {
			x--
		} else if still >= 2 && r > 0.78 && x < front-2 {
			x++
		}
		if x == prev {
			still++
		} else {
			still = 0
		}
		seam.Set(x, y, seamCore)
		if prev != x { // the step itself glows
			seam.Set(prev, y, seamMid)
		}
		if hash(x, y, s.Seed+9) > 0.55 {
			side := 1
			if hash(x, y, s.Seed+11) > 0.5 {
				side = -1
			}
			if seam.At(x+side, y) == "" && x+side > 1 && x+side <= front {
				seam.Set(x+side, y, seamMid)
			}
		}
		for dx := -2; dx <= 2; dx++ {
			if dx == 0 || x+dx < 2 || x+dx > front {
				continue
			}
			near := dx == -1 || dx == 1
			switch r := hash(x+dx, y, s.Seed+23); {
			case near && r < 0.7:
				halo.Set(x+dx, y, warmNear)
			case !near && r < 0.35:
				halo.Set(x+dx, y, warmFar)
			}
		}
		prev = x
	}
	if s.Branches {
		for _, start := range []int{s.H / 3, s.H / 2, 2 * s.H / 3} {
			// a short crack off the seam, going down and outward
			sx := -1
			for xx := 0; xx < s.W; xx++ {
				if seam.At(xx, start) == seamCore {
					sx = xx
				}
			}
			if sx < 0 {
				continue
			}
			dir := 1
			if hash(sx, start, s.Seed+13) > 0.5 {
				dir = -1
			}
			cx, cy := sx, start
			for i := 0; i < 4+int(hash(sx, start, s.Seed+17)*4); i++ {
				cx += dir
				if hash(cx, cy, s.Seed+19) > 0.45 {
					cy++
				}
				if cx < 2 || cx > front-1 || cy >= s.H-1 {
					break
				}
				c := seamEdge
				if i < 2 {
					c = seamMid
				}
				seam.Set(cx, cy, c)
			}
		}
	}
	return stone, halo, seam
}

// glowCSS dims and brightens the seam where the SVG is a file. Inlined, the
// page's stylesheet drives the same classes from --lith-seam-glow.
const glowCSS = `.px-art .halo{animation:px-glow 5s ease-in-out infinite alternate}` +
	`.px-art .seam{animation:px-seam 5s ease-in-out infinite alternate}` +
	`@keyframes px-glow{from{opacity:.35}to{opacity:1}}` +
	`@keyframes px-seam{from{opacity:.8}to{opacity:1}}` +
	`@media (prefers-reduced-motion:reduce){.px-art *{animation:none!important}}`

func logoSlab() slab { return slab{W: 12, H: 22, Side: 3, Top: 2, Seed: 4} }

// Logo is the small monolith of the header and the favicon.
func Logo() string {
	stone, halo, seam := logoSlab().layers()
	return SVG(stone.W, stone.H, glowCSS, Layer{Grid: stone}, Layer{Grid: halo, Class: "halo"}, Layer{Grid: seam, Class: "seam"})
}

func heroSlab() slab { return slab{W: 22, H: 60, Side: 5, Top: 3, Seed: 21, Branches: true} }

// Hero is the large monolith of the landing page, standing on a strip of
// ground with a few stones at its foot.
func Hero() string {
	w, h, layers := heroLayers()
	return SVG(w, h, glowCSS, layers...)
}

const heroW, heroH = 44, 64

func heroLayers() (int, int, []Layer) {
	s := heroSlab()
	stone, halo, seam := s.layers()
	ox, oy := (heroW-s.W)/2, 1
	ground := NewGrid(heroW, heroH)
	foot := oy + s.H - 1
	for x := 0; x < heroW; x++ {
		// the ground's lit rim, then earth that fades out to the sides
		d := float64(x-heroW/2) / float64(heroW/2)
		if d*d > 0.9 {
			continue
		}
		ground.Set(x, foot, stoneDim)
		for y := foot + 1; y < heroH; y++ {
			if hash(x, y, 31) > 0.35+0.5*d*d+0.08*float64(y-foot) {
				ground.Set(x, y, stoneSide)
			}
		}
	}
	for _, p := range [][2]int{{ox - 3, foot - 1}, {ox - 1, foot - 1}, {ox + s.W + 1, foot - 1}, {ox + s.W + 3, foot - 1}, {ox - 6, foot - 1}} {
		ground.Set(p[0], p[1], stoneFace)
		ground.Set(p[0]+1, p[1], stoneDim)
	}
	glowOnGround := NewGrid(heroW, heroH)
	for x := ox + 2; x < ox+s.W-s.Side-1; x++ {
		if hash(x, foot, 41) > 0.3 {
			glowOnGround.Set(x, foot, warmNear)
		}
	}
	return heroW, heroH, []Layer{
		{Grid: ground},
		{Grid: stone, X: ox, Y: oy},
		{Grid: halo, X: ox, Y: oy, Class: "halo"},
		{Grid: glowOnGround, Class: "halo"},
		{Grid: seam, X: ox, Y: oy, Class: "seam"},
	}
}
