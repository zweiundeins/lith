package cursorparty

import (
	"html"
	"math"
	"strconv"
	"time"
)

// layer is the party's layer: one <i> for each cursor, with its place in
// percent of the party's width and height (--x, --y), its colour (data-h)
// and its name tag (<b>). The view renders it in every tick with a move,
// so it is written by hand, about 90 bytes a cursor, and CSS draws the
// arrow and the trail. A cursor quiet for Idle carries data-idle and
// fades.
func layer(list []Cursor, now time.Time) []byte {
	b := make([]byte, 0, 80+len(list)*96)
	b = append(b, `<div id="cursor-party-cursors" class="cp-layer" aria-hidden="true">`...)
	for _, c := range list {
		b = append(b, `<i id="cursor-party-`...)
		b = append(b, c.ID...)
		b = append(b, `" data-h="`...)
		b = strconv.AppendInt(b, int64(c.Hue), 10)
		b = append(b, `" style="--x:`...)
		b = percent(b, c.X)
		b = append(b, `;--y:`...)
		b = percent(b, c.Y)
		b = append(b, '"')
		if now.Sub(c.At) >= Idle {
			b = append(b, ` data-idle`...)
		}
		b = append(b, `><b>`...)
		b = append(b, html.EscapeString(c.Name)...)
		b = append(b, `</b></i>`...)
	}
	return append(b, `</div>`...)
}

// percent writes a fraction as a percentage with one decimal: a tenth of
// a percent is 1.4 pixels of a 1440 pixel wide party.
func percent(b []byte, f float64) []byte {
	return strconv.AppendFloat(b, math.Round(f*1000)/10, 'f', 1, 64)
}

// selfStyle hides the viewer's own cursor: the pointer it sees is its own,
// never one drawn back from the server. The page renders it once; the
// layer's frames are the same for every tab.
func selfStyle(tab string) string {
	id, _, _ := Identity(tab)
	return `#cursor-party-` + id + `{display:none}`
}
