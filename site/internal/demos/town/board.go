package town

import (
	"context"
	"html"
	"io"
	"strconv"
	"strings"

	"github.com/a-h/templ"
)

// people are the builders a frame names, each drawn once per render: an
// avatar is a few dozen allocations, and a town of 96 tiles names a
// handful of people many times.
type people map[string]person

func (p people) of(who string) person {
	if x, ok := p[who]; ok {
		return x
	}
	x := personOf(who)
	p[who] = x
	return x
}

// Board is the tiles, written by hand: every frame of a position carries
// all 96, and a render a tick per position is the cost of a dragged
// slider. At a position the tiles take no clicks (aria-disabled) and carry
// no versions: nothing is built in the past. Only tile 0 is in the tab
// order; the arrow keys go from tile to tile (expr.go, keys).
func Board(m moment) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		var b strings.Builder
		b.Grow(16 << 10)
		writeBoard(&b, &m)
		_, err := io.WriteString(w, b.String())
		return err
	})
}

func writeBoard(b *strings.Builder, m *moment) {
	b.WriteString(`<div id="town-map" class="town-map" data-at="`)
	b.WriteString(strconv.FormatInt(m.At, 10))
	b.WriteString(`" role="group" aria-label="`)
	b.WriteString(boardLabel(*m))
	b.WriteString(`">`)
	for i := range Tiles {
		t := m.Town[i]
		b.WriteString(`<button type="button" class="town-tile" data-t="`)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`" data-k="`)
		b.WriteString(kindAttr(t.Kind))
		if m.At == 0 {
			b.WriteString(`" data-v="`)
			b.WriteString(strconv.FormatInt(t.Version, 10))
		} else {
			b.WriteString(`" aria-disabled="true`)
		}
		if t.Kind == House {
			b.WriteString(`" style="--h: `)
			b.WriteString(strconv.Itoa(m.people.of(t.Who).Hue))
		}
		if i == 0 {
			b.WriteString(`" tabindex="0" aria-label="`)
		} else {
			b.WriteString(`" tabindex="-1" aria-label="`)
		}
		b.WriteString(label(i))
		b.WriteString(", ")
		b.WriteString(kindPhrase(t.Kind))
		if t.Kind != Grass {
			b.WriteString(", by ")
			b.WriteString(html.EscapeString(m.people.of(t.Who).Name))
		}
		b.WriteString(`">`)
		writeGlyph(b, &m.Town, i)
		b.WriteString(`</button>`)
	}
	b.WriteString(`</div>`)
}
