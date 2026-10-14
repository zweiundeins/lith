package cursorparty

import (
	"encoding/json"
	"strings"
)

// An Anchor is a box a cursor is pinned to. The landing page's hero marks
// its anchors (data-cp-anchor): the four lines of the acrostic, the
// monolith, the lede and the buttons. A move names the anchor nearest the
// pointer, and its X and Y are fractions of that anchor's box, so a cursor
// lands by the same words on a phone and on a desktop. Home is where the
// desktop hero has the box, in fractions of the floor (x, y, width,
// height): a page without the anchor, as the demo's own floor, or with it
// hidden, puts it there.
type Anchor struct {
	Name string
	Home [4]float64
}

// floorAnchor is the floor itself: rows from before anchors are on it,
// and so is a move that names no anchor.
const floorAnchor = "floor"

// Anchors are the hero's, measured on a 1440 by 900 window.
var Anchors = []Anchor{
	{"L", [4]float64{0.0778, 0.1265, 0.2343, 0.1071}},
	{"I", [4]float64{0.0778, 0.2443, 0.2723, 0.1071}},
	{"T", [4]float64{0.0778, 0.3621, 0.0998, 0.1071}},
	{"H", [4]float64{0.0778, 0.4799, 0.2008, 0.1071}},
	{"art", [4]float64{0.6545, 0.1228, 0.2292, 0.7301}},
	{"lede", [4]float64{0.0778, 0.6235, 0.4, 0.1697}},
	{"go", [4]float64{0.0778, 0.8418, 0.5104, 0.0608}},
}

func knownAnchor(name string) bool {
	for _, a := range Anchors {
		if a.Name == name {
			return true
		}
	}
	return name == floorAnchor || name == ""
}

// anchorHomes is the anchors' homes as a JSON object, for the floor's
// script.
func anchorHomes() string {
	m := make(map[string][4]float64, len(Anchors))
	names := make([]string, len(Anchors))
	for i, a := range Anchors {
		m[a.Name] = a.Home
		names[i] = a.Name
	}
	b, _ := json.Marshal(m)
	n, _ := json.Marshal(names)
	return strings.NewReplacer("NAMES", string(n), "HOMES", string(b)).Replace(measure)
}
