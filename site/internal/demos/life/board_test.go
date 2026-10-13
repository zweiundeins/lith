package life

import (
	"math/rand/v2"
	"testing"
)

// place returns an empty board with the live cells of rows put with their
// top left corner at x, y, in colour.
func place(x, y int, colour byte, rows ...string) []byte {
	cells := make([]byte, Cells)
	for j, r := range rows {
		for i, c := range r {
			if c == 'O' {
				cells[at(x+i, y+j)] = colour
			}
		}
	}
	return cells
}

// run advances cells by n generations.
func run(cells []byte, n int) []byte {
	cur := append([]byte(nil), cells...)
	next := make([]byte, Cells)
	for g := range n {
		Next(next, cur, int64(g))
		cur, next = next, cur
	}
	return cur
}

func same(t *testing.T, what string, got, want []byte) {
	t.Helper()
	if string(got) != string(want) {
		t.Errorf("%s:\n%s\nwant:\n%s", what, Plain(got), Plain(want))
	}
}

func TestStillLifeAndBlinker(t *testing.T) {
	block := place(10, 10, 1, "OO", "OO")
	same(t, "a block", run(block, 1), block)

	blinker := place(20, 20, 1, "OOO")
	same(t, "a blinker after one", run(blinker, 1), place(21, 19, 1, "O", "O", "O"))
	same(t, "a blinker after two", run(blinker, 2), blinker)
}

// The board wraps around: a blinker across the left and right edges, and
// one across the top and bottom, blink as anywhere else.
func TestTorus(t *testing.T) {
	across := place(W-1, 5, 1, "OOO")
	same(t, "across the sides", run(across, 1), place(0, 4, 1, "O", "O", "O"))
	same(t, "across the sides, twice", run(across, 2), across)

	down := place(30, H-1, 1, "O", "O", "O")
	same(t, "across top and bottom", run(down, 1), place(29, 0, 1, "OOO"))

	// A glider crosses a corner: 4 generations later it is one cell down
	// and to the right, in four pieces at the board's corners.
	glider := place(W-2, H-2, 1, ".O.", "..O", "OOO")
	same(t, "a glider across a corner", run(glider, 4), place(W-1, H-1, 1, ".O.", "..O", "OOO"))
}

// Each pattern does what its button says: the glider and the spaceship
// move, the pulsar pulses with a period of three, the gun fires a glider
// every thirty generations.
func TestPatterns(t *testing.T) {
	shapes := map[string][]byte{}
	for _, p := range Patterns {
		cells := make([]byte, Cells)
		p.Stamp(cells, at(40, 25), 0, 1)
		shapes[p.Name] = cells
	}
	g, _ := Find("glider")
	moved := make([]byte, Cells)
	g.Stamp(moved, at(41, 26), 0, 1)
	same(t, "the glider after 4", run(shapes["glider"], 4), moved)

	s, _ := Find("spaceship")
	moved = make([]byte, Cells)
	s.Stamp(moved, at(42, 25), 0, 1)
	same(t, "the spaceship after 4", run(shapes["spaceship"], 4), moved)

	same(t, "the pulsar after 3", run(shapes["pulsar"], 3), shapes["pulsar"])
	if p := Population(run(shapes["pulsar"], 1)); p == 48 {
		t.Error("the pulsar does not pulse")
	}

	gun := shapes["gun"]
	if n := Population(gun); n != 36 {
		t.Errorf("the gun has %d cells", n)
	}
	for _, gens := range []int{30, 60} {
		if n := Population(run(gun, gens)); n != 36+5*gens/30 {
			t.Errorf("the gun after %d generations: %d cells, want %d", gens, n, 36+5*gens/30)
		}
	}
}

// A pattern turned a quarter clockwise moves turned too: the glider that
// crawls down and to the right crawls down and to the left.
func TestTurn(t *testing.T) {
	g, _ := Find("glider")
	cells := make([]byte, Cells)
	g.Stamp(cells, at(40, 25), 1, 1)
	want := make([]byte, Cells)
	g.Stamp(want, at(39, 26), 1, 1)
	same(t, "a turned glider after 4", run(cells, 4), want)

	four := make([]byte, Cells)
	g.Stamp(four, at(40, 25), 4, 1)
	plain := make([]byte, Cells)
	g.Stamp(plain, at(40, 25), 0, 1)
	same(t, "four quarters", four, plain)
}

// A stamp clears its box, and wraps around the board.
func TestStamp(t *testing.T) {
	cells := make([]byte, Cells)
	for i := range cells {
		cells[i] = Wild
	}
	g, _ := Find("glider")
	if n := g.Stamp(cells, at(0, 0), 0, 3); n != 9 {
		t.Errorf("a glider's box has %d cells", n)
	}
	if got := Population(cells); got != Cells-9+5 {
		t.Errorf("after the stamp, %d live", got)
	}
	// The anchor is the box's middle cell: the box spans -1..1 around it.
	if cells[at(-1, -1)] != 0 || cells[at(0, -1)] != 3 || cells[at(1, 0)] != 3 {
		t.Error("the stamp is not centred on its anchor across the corner")
	}
}

// A newborn cell takes the colour most of its parents have; three
// different parents give one of theirs, the same for the same board.
func TestColours(t *testing.T) {
	cells := make([]byte, Cells)
	cells[at(10, 10)], cells[at(11, 10)], cells[at(12, 10)] = 1, 2, 2 // a blinker, two parts blue
	next := make([]byte, Cells)
	Next(next, cells, 0)
	if next[at(11, 9)] != 2 || next[at(11, 11)] != 2 || next[at(11, 10)] != 2 {
		t.Errorf("born of 1, 2, 2: %d and %d; the middle survives as %d", next[at(11, 9)], next[at(11, 11)], next[at(11, 10)])
	}
	cells[at(12, 10)] = 3
	Next(next, cells, 0)
	a := next[at(11, 9)]
	if a != 1 && a != 2 && a != 3 {
		t.Errorf("born of 1, 2, 3: %d", a)
	}
	again := make([]byte, Cells)
	Next(again, cells, 0)
	if string(again) != string(next) {
		t.Error("the same board has two next generations")
	}
}

func TestLine(t *testing.T) {
	cells := make([]byte, Cells)
	if n := Line(cells, at(10, 10), at(14, 12), 4); n != 5 {
		t.Errorf("a line of 5 columns set %d cells", n)
	}
	for _, c := range [][2]int{{10, 10}, {14, 12}, {12, 11}} {
		if cells[at(c[0], c[1])] != 4 {
			t.Errorf("the line misses %v", c)
		}
	}
	if n := Line(cells, at(3, 3), at(3, 3), 0); n != 1 {
		t.Errorf("a point set %d cells", n)
	}
}

func TestSoup(t *testing.T) {
	cells := make([]byte, Cells)
	n := Soup(cells, rand.New(rand.NewPCG(1, 2)))
	if n < Cells/4 || n > Cells*2/5 || Population(cells) != n || Counts(cells)[Wild] != n {
		t.Errorf("a soup of %d cells", n)
	}
	if Hash(cells) == Hash(make([]byte, Cells)) {
		t.Error("the soup hashes like an empty board")
	}
}

func BenchmarkNext(b *testing.B) {
	cur := make([]byte, Cells)
	Soup(cur, rand.New(rand.NewPCG(1, 2)))
	next := make([]byte, Cells)
	for i := range b.N {
		Next(next, cur, int64(i))
		cur, next = next, cur
	}
}
