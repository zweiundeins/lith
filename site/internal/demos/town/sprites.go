package town

import (
	"strings"

	"github.com/a-h/templ"

	"github.com/zweiundeins/lith/site/internal/pixelart"
)

// The tiles' pixel art, 8 by 8, drawn once per page as symbols that every
// tile uses by reference: a frame carries a tile's kind, not its pixels.
// Colours are the demo's custom properties (town.css), so the town
// follows the theme; a house's roof is its builder's hue (--h), and roads
// and bridges share their pieces, in asphalt or in wood (--_pave).
var palette = map[rune]string{
	'R': "var(--_roof)",
	'r': "var(--_roof-2)",
	'W': "var(--town-wall)",
	'w': "var(--town-wall-2)",
	'D': "var(--town-door)",
	'G': "var(--town-window)",
	'L': "var(--town-leaf)",
	'l': "var(--town-leaf-2)",
	'T': "var(--town-trunk)",
	'~': "var(--town-wave)",
	'P': "var(--_pave)",
}

var symbols = []struct {
	id   string
	rows []string
}{
	{"house", []string{
		"...RR...",
		"..RRRr..",
		".RRRRrr.",
		"RRRRRrrr",
		".WWWWWw.",
		".WGWWDw.",
		".WWWWDw.",
		"........",
	}},
	{"tree", []string{
		"...LL...",
		"..LlLL..",
		".LlLLLL.",
		".LLLLlL.",
		"..LLlL..",
		"...TT...",
		"...TT...",
		"........",
	}},
	{"water", []string{
		"........",
		".~~.....",
		"~..~..~~",
		"....~~..",
		"........",
		"..~~....",
		".~..~.~~",
		"......~.",
	}},
	{"road", []string{
		"........",
		"........",
		"..PPPP..",
		"..PPPP..",
		"..PPPP..",
		"..PPPP..",
		"........",
		"........",
	}},
	{"arm-n", []string{
		"..PPPP..",
		"..PPPP..",
	}},
	{"arm-s", []string{
		"........",
		"........",
		"........",
		"........",
		"........",
		"........",
		"..PPPP..",
		"..PPPP..",
	}},
	{"arm-w", []string{
		"........",
		"........",
		"PP......",
		"PP......",
		"PP......",
		"PP......",
	}},
	{"arm-e", []string{
		"........",
		"........",
		"......PP",
		"......PP",
		"......PP",
		"......PP",
	}},
}

// spriteSheet is the symbols in a hidden SVG, rendered once.
var spriteSheet = func() templ.Component {
	var b strings.Builder
	b.WriteString(`<svg class="town-sheet" aria-hidden="true" focusable="false"><defs>`)
	for _, s := range symbols {
		b.WriteString(`<symbol id="town-` + s.id + `" viewBox="0 0 8 8">`)
		b.WriteString(pixelart.Paths(pixelart.Sprite(palette, s.rows...)))
		b.WriteString(`</symbol>`)
	}
	b.WriteString(`</defs></svg>`)
	return templ.Raw(b.String())
}()

// paved reports whether a tile carries a road, as a road or a bridge.
func paved(kind string) bool { return kind == Road || kind == Bridge }

// writeGlyph writes a tile's picture: the symbols it uses, a road's arms
// toward its paved neighbours. Grass has none.
func writeGlyph(b *strings.Builder, t *Town, i int) {
	k := t[i].Kind
	switch k {
	case Grass:
		return
	case House, Tree, Water:
		b.WriteString(`<svg viewBox="0 0 8 8" aria-hidden="true"><use href="#town-`)
		b.WriteString(k)
		b.WriteString(`"></use></svg>`)
		return
	}
	b.WriteString(`<svg viewBox="0 0 8 8" aria-hidden="true">`)
	if k == Bridge {
		b.WriteString(`<use href="#town-water"></use>`)
	}
	b.WriteString(`<use href="#town-road"></use>`)
	col := i % Cols
	if i >= Cols && paved(t[i-Cols].Kind) {
		b.WriteString(`<use href="#town-arm-n"></use>`)
	}
	if col < Cols-1 && paved(t[i+1].Kind) {
		b.WriteString(`<use href="#town-arm-e"></use>`)
	}
	if i+Cols < Tiles && paved(t[i+Cols].Kind) {
		b.WriteString(`<use href="#town-arm-s"></use>`)
	}
	if col > 0 && paved(t[i-1].Kind) {
		b.WriteString(`<use href="#town-arm-w"></use>`)
	}
	b.WriteString(`</svg>`)
}

// toolGlyph is a tool's picture in the palette.
func toolGlyph(kind string) string {
	switch kind {
	case Grass:
		return ""
	case Road:
		return `<svg viewBox="0 0 8 8"><use href="#town-road"></use><use href="#town-arm-w"></use><use href="#town-arm-e"></use></svg>`
	}
	return `<svg viewBox="0 0 8 8"><use href="#town-` + kind + `"></use></svg>`
}
