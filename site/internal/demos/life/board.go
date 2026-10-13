package life

import (
	"math/rand/v2"
	"strings"
)

// The board is W×H cells on a torus: its edges wrap around. It is one byte
// per cell, row by row: 0 for a dead cell, and for a live one its colour,
// 1 to Colours for a person's, or Wild for the soup's.
const (
	W       = 80
	H       = 50
	Cells   = W * H
	Colours = 8
	Wild    = byte(Colours + 1)
)

// Next writes the generation after cur into next and returns how many
// cells live in it: Conway's rules, B3/S23. A cell that survives keeps its
// colour. A cell born takes the colour of most of its three parents, and
// when all three differ, the colour of one of them, picked by the cell and
// the generation, so that the same board always has the same next one.
func Next(next, cur []byte, generation int64) (population int) {
	for y := range H {
		up, row, down := (y+H-1)%H*W, y*W, (y+1)%H*W
		for x := range W {
			left, right := (x+W-1)%W, (x+1)%W
			var parents [3]byte
			n := 0
			for _, i := range [8]int{up + left, up + x, up + right, row + left, row + right, down + left, down + x, down + right} {
				if c := cur[i]; c != 0 {
					if n < 3 {
						parents[n] = c
					}
					n++
				}
			}
			i := row + x
			switch c := cur[i]; {
			case c != 0 && (n == 2 || n == 3):
				next[i] = c
			case c == 0 && n == 3:
				next[i] = majority(parents, (int64(i)+generation)%3)
			default:
				next[i] = 0
			}
			if next[i] != 0 {
				population++
			}
		}
	}
	return population
}

// majority is the colour two or three of the parents share, or else the
// parent at index tie.
func majority(p [3]byte, tie int64) byte {
	switch {
	case p[0] == p[1] || p[0] == p[2]:
		return p[0]
	case p[1] == p[2]:
		return p[1]
	}
	return p[tie]
}

// Population counts the live cells.
func Population(cells []byte) (n int) {
	for _, c := range cells {
		if c != 0 {
			n++
		}
	}
	return n
}

// Hash is the FNV-1a hash of the cells: two boards with the same hash are
// taken to be the same board.
func Hash(cells []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, c := range cells {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return h
}

// Soup fills the board with a random soup in the soup's colour, about a
// third of the cells alive, and returns how many live.
func Soup(cells []byte, rng *rand.Rand) (population int) {
	for i := range cells {
		cells[i] = 0
		if rng.IntN(100) < soupDensity {
			cells[i] = Wild
			population++
		}
	}
	return population
}

const soupDensity = 33 // percent

// at is the index of the cell at x, y, both wrapped onto the board.
func at(x, y int) int { return (y%H+H)%H*W + (x%W+W)%W }

// Line sets the cells on the straight line from one cell to another, both
// ends included, to colour (0 kills them), and returns how many it set.
func Line(cells []byte, from, to int, colour byte) int {
	x0, y0, x1, y1 := from%W, from/W, to%W, to/W
	dx, dy := abs(x1-x0), -abs(y1-y0)
	sx, sy := sign(x1-x0), sign(y1-y0)
	e := dx + dy
	n := 0
	for {
		cells[at(x0, y0)] = colour
		n++
		if x0 == x1 && y0 == y1 {
			return n
		}
		e2 := 2 * e
		if e2 >= dy {
			e += dy
			x0 += sx
		}
		if e2 <= dx {
			e += dx
			y0 += sy
		}
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// Pattern is a shape someone can drop on the board. Its anchor, the cell
// that lands where they tap and that it turns around, is the cell at the
// middle of its box, rounded up and to the left.
type Pattern struct {
	Name  string // in URLs and payloads
	Title string
	About string // what it does, one line
	W, H  int
	alive []bool
}

func pattern(name, title, about string, rows ...string) Pattern {
	p := Pattern{Name: name, Title: title, About: about, W: len(rows[0]), H: len(rows)}
	for _, r := range rows {
		if len(r) != p.W {
			panic("life: the rows of " + name + " differ in length")
		}
		for _, c := range r {
			p.alive = append(p.alive, c == 'O')
		}
	}
	return p
}

// Alive reports whether the cell at x, y of the pattern's box lives.
func (p Pattern) Alive(x, y int) bool { return p.alive[y*p.W+x] }

// Anchor is the cell of the box that lands on the tapped cell.
func (p Pattern) Anchor() (x, y int) { return p.W / 2, p.H / 2 }

// Stamp draws the pattern with its anchor on cell, turned clockwise by
// turn quarters, in colour: its live cells live and the rest of its box
// is cleared, so that it starts as it should. It returns how many cells
// it set.
func (p Pattern) Stamp(cells []byte, cell, turn int, colour byte) int {
	ax, ay := p.Anchor()
	cx, cy := cell%W, cell/W
	n := 0
	for y := range p.H {
		for x := range p.W {
			dx, dy := x-ax, y-ay
			for range turn & 3 {
				dx, dy = -dy, dx // a quarter turn clockwise, y pointing down
			}
			var c byte
			if p.Alive(x, y) {
				c = colour
			}
			cells[at(cx+dx, cy+dy)] = c
			n++
		}
	}
	return n
}

// Patterns are the shapes on the toolbar, by name.
var Patterns = []Pattern{
	pattern("glider", "Glider", "Five cells that crawl one cell diagonally every four generations.",
		".O.",
		"..O",
		"OOO",
	),
	pattern("spaceship", "Spaceship", "The lightweight spaceship: nine cells that fly two cells sideways every four generations.",
		"O..O.",
		"....O",
		"O...O",
		".OOOO",
	),
	pattern("pulsar", "Pulsar", "48 cells that pulse in place, a period of three generations.",
		"..OOO...OOO..",
		".............",
		"O....O.O....O",
		"O....O.O....O",
		"O....O.O....O",
		"..OOO...OOO..",
		".............",
		"..OOO...OOO..",
		"O....O.O....O",
		"O....O.O....O",
		"O....O.O....O",
		".............",
		"..OOO...OOO..",
	),
	pattern("gun", "Glider gun", "Bill Gosper's gun: it fires a new glider every thirty generations.",
		"........................O...........",
		"......................O.O...........",
		"............OO......OO............OO",
		"...........O...O....OO............OO",
		"OO........O.....O...OO..............",
		"OO........O...O.OO....O.O...........",
		"..........O.....O.......O...........",
		"...........O...O....................",
		"............OO......................",
	),
}

// Find returns the pattern with the name.
func Find(name string) (Pattern, bool) {
	for _, p := range Patterns {
		if p.Name == name {
			return p, true
		}
	}
	return Pattern{}, false
}

// Plain draws cells as text, one row per line, '.' dead and 'O' alive: for
// tests and their failures.
func Plain(cells []byte) string {
	var b strings.Builder
	for y := range H {
		for x := range W {
			if cells[y*W+x] != 0 {
				b.WriteByte('O')
			} else {
				b.WriteByte('.')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
