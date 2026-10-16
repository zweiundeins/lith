package town

import (
	"strconv"
	"time"

	"github.com/zweiundeins/lith/site/internal/demos/board"
)

// label names a tile as a map does: its column's letter and its row's
// number, "C4".
func label(i int) string {
	return string(rune('A'+i%Cols)) + strconv.Itoa(i/Cols+1)
}

// kindPhrase is a kind of tile in a sentence.
func kindPhrase(kind string) string {
	switch kind {
	case House:
		return "a house"
	case Tree:
		return "a tree"
	case Road:
		return "a road"
	case Bridge:
		return "a bridge"
	case Water:
		return "water"
	}
	return "grass"
}

// toolName is a tool's name in the palette.
func toolName(kind string) string {
	switch kind {
	case House:
		return "House"
	case Tree:
		return "Tree"
	case Road:
		return "Road"
	case Water:
		return "Water"
	}
	return "Clear"
}

// person is who caused an event: the board's avatar of their seed, or the
// town itself, which laid out the start.
type person struct {
	Name string
	Hue  int
	Path string
}

func personOf(who string) person {
	if who == "" {
		return person{Name: "The town", Hue: 30, Path: "M2 1h3v1h-3zM1 2h5v1h-5zM2 3h3v3h-3z"}
	}
	a := board.AvatarOf(who)
	return person{Name: a.Name, Hue: a.Hue, Path: a.Path}
}

func nameOf(who string) string { return personOf(who).Name }

// hue is the hue of a house's roof: its builder's.
func hue(who string) string { return "--h: " + strconv.Itoa(personOf(who).Hue) }

// what says what an event did, after its builder's name.
func what(e Event) string {
	at := label(e.Tile)
	switch d := e.Data.(type) {
	case Built:
		var s string
		switch d.Kind {
		case House:
			s = "built a house on " + at
		case Tree:
			s = "planted a tree on " + at
		case Road:
			s = "laid a road on " + at
		case Water:
			s = "flooded " + at
		case Bridge:
			return "bridged the water on " + at
		}
		if d.Over != Grass {
			s += ", where " + kindPhrase(d.Over) + " was"
		}
		return s
	case Cleared:
		return "cleared " + kindPhrase(d.Was) + " from " + at
	}
	return "changed " + at
}

func clock(t time.Time) string { return t.UTC().Format("15:04:05") }

func stamp(t time.Time) string { return t.UTC().Format("2 Jan 2006, 15:04:05 UTC") }

func grouped(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// kindAttr is a kind as the page's CSS selects it.
func kindAttr(kind string) string {
	if kind == Grass {
		return "grass"
	}
	return kind
}

// boardLabel names the town's region for a screen reader.
func boardLabel(m moment) string {
	if m.At == 0 {
		return "The town now"
	}
	return "The town at position " + grouped(m.At)
}
