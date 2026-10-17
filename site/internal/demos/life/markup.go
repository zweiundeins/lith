package life

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// Markup is another rendering of the same board, for measuring what a
// frame's markup costs the page (TestLifeDrift's LIFE_DRIFT_RENDER and
// LIFE_DRIFT_STATS). The site always serves Demo; only that test serves
// Markup.Demo.
type Markup struct {
	// Board is "cells" (as the site renders it), "lean" (one bare <p> per
	// cell, no rows, a class on live and fading cells only), "svg" (one
	// <path> per colour and age) or "canvas" (a signal of one letter per
	// cell, drawn by a data-effect). "term" and "termruns" are not the
	// board: an 80 by 24 terminal whose every cell changes each frame, one
	// <span> per cell, or one per run of the same colours.
	Board string
	// NoStats leaves the counters, the shares and the status out of the
	// frames: the page renders them once.
	NoStats bool
}

// Markups are the boards Markup knows.
var Markups = []string{"cells", "lean", "svg", "canvas", "term", "termruns"}

// Demo is the Game of Life with this markup.
func (m Markup) Demo() (demo.Demo, error) {
	switch m.Board {
	case "cells", "lean", "svg", "canvas", "term", "termruns":
	default:
		return demo.Demo{}, fmt.Errorf("life: no markup %q", m.Board)
	}
	d := Demo
	d.CSS = css + markupCSS
	d.Page.Render = func(ctx context.Context, tx *sql.Tx, v demo.Viewer) (templ.Component, error) {
		q := demo.Querier(ctx, tx)
		b, err := load(ctx, q)
		if err != nil {
			return nil, err
		}
		colour, err := colourOf(ctx, q, v.Session)
		if err != nil {
			return nil, err
		}
		var page, cells strings.Builder
		if err := Stage(b, demo.Watching(ctx, BoardKey), colour, time.Now()).Render(ctx, &page); err != nil {
			return nil, err
		}
		boardCells(b.Cells, b.Trail).Render(ctx, &cells)
		return templ.Raw(strings.Replace(page.String(), cells.String(), m.board(b), 1)), nil
	}
	d.Page.Views = func(v demo.Viewer) []live.View {
		board := boardView()
		board.Render = func(ctx context.Context, tx *sql.Tx) (any, error) {
			b, err := load(ctx, demo.Querier(ctx, tx))
			if err != nil {
				return nil, err
			}
			return m.frame(ctx, b, demo.Watching(ctx, BoardKey), time.Now())
		}
		return []live.View{board, tabView(v)}
	}
	return d, nil
}

// frame is one frame of the board's view.
func (m Markup) frame(ctx context.Context, b Board, here int, now time.Time) ([]byte, error) {
	var out []byte
	var els strings.Builder
	if m.Board == "canvas" {
		out = sse.PatchSignals(canvasSignals(b), false)
	} else {
		els.WriteString(m.board(b))
	}
	if !m.NoStats {
		for _, c := range []templ.Component{Stats(b, here), Share(b.Cells), Status(b, now)} {
			if err := c.Render(ctx, &els); err != nil {
				return nil, err
			}
		}
	}
	if els.Len() > 0 {
		out = append(out, sse.PatchElements([]byte(els.String()))...)
	}
	return out, nil
}

// board is the board's element in this markup. Every one but the site's
// own with its counters carries the generation in data-gen, where
// TestLifeDrift reads it.
func (m Markup) board(b Board) string {
	gen := strconv.FormatInt(b.Generation, 10)
	switch m.Board {
	case "lean":
		return leanBoard(b, gen)
	case "svg":
		return svgBoard(b, gen)
	case "term", "termruns":
		return terminal(b.Generation, m.Board == "termruns")
	case "canvas":
		sig, _ := json.Marshal(json.RawMessage(canvasSignals(b)))
		return `<canvas id="life-board" class="life__board life__board--canvas" aria-hidden="true" data-signals="` +
			html.EscapeString(string(sig)) + `" data-effect="` + html.EscapeString(drawCanvas) + `"></canvas>`
	}
	var s strings.Builder
	boardCells(b.Cells, b.Trail).Render(context.Background(), &s)
	if m.NoStats {
		return strings.Replace(s.String(), `aria-hidden="true">`, `aria-hidden="true" data-gen="`+gen+`">`, 1)
	}
	return s.String()
}

// codes is the class letter of every cell: _ for a dead cell without a
// trail, else the letter boardCells gives it.
func codes(b Board) []byte {
	out := make([]byte, Cells)
	for i, c := range b.Cells {
		if t := trailAt(b.Trail, i); c == 0 && t != 0 {
			out[i] = [...]string{cellClass, dyingClass, trailClass}[t>>4][t&15]
			continue
		}
		out[i] = cellClass[c]
	}
	return out
}

// leanBoard is the board as a Datastar user would write it with the least
// markup: 4,000 <p> in one grid, a class only where a cell is not plain
// dead, and no ids but the board's.
func leanBoard(b Board, gen string) string {
	var s strings.Builder
	s.WriteString(`<div id="life-board" class="life__board life__board--lean" aria-hidden="true" data-gen="` + gen + `">`)
	for _, k := range codes(b) {
		if k == '_' {
			s.WriteString("<p>")
			continue
		}
		s.WriteString("<p class=")
		s.WriteByte(k)
		s.WriteByte('>')
	}
	s.WriteString("</div>")
	return s.String()
}

// letters are the class letters in a fixed order: live, dying, trail.
const letters = "abcdefghwABCDEFGHWijklmnopx"

// svgBoard is the board as one SVG: a pattern of dots for the dead cells
// and a <path> of each letter's cells, each cell a zero-length line whose
// square cap is the cell.
func svgBoard(b Board, gen string) string {
	var paths [len(letters)]strings.Builder
	for i, k := range codes(b) {
		j := strings.IndexByte(letters, k)
		if j < 0 {
			continue
		}
		p := &paths[j]
		p.WriteByte('M')
		p.WriteString(strconv.Itoa(i % W))
		p.WriteByte(' ')
		p.WriteString(strconv.Itoa(i / W))
		p.WriteString("h0")
	}
	var s strings.Builder
	s.WriteString(`<div id="life-board" class="life__board life__board--svg" aria-hidden="true" data-gen="` + gen + `">`)
	s.WriteString(`<svg viewBox="0 0 80 50" preserveAspectRatio="none"><pattern id="life-dots" width="1" height="1" patternUnits="userSpaceOnUse"><rect x=".38" y=".38" width=".24" height=".24"></rect></pattern><rect class="dots" width="80" height="50" fill="url(#life-dots)"></rect><g transform="translate(.5 .5)">`)
	for j := range paths {
		if paths[j].Len() == 0 {
			continue
		}
		s.WriteString(`<path class=` + letters[j:j+1] + ` d="` + paths[j].String() + `"></path>`)
	}
	s.WriteString(`</g></svg></div>`)
	return s.String()
}

// canvasSignals is the board as a signal: its generation, and one letter
// per cell.
func canvasSignals(b Board) []byte {
	return []byte(`{"_lifeb":{"g":` + strconv.FormatInt(b.Generation, 10) + `,"c":"` + string(codes(b)) + `"}}`)
}

// drawCanvas draws $_lifeb into the canvas: each cell a square of its
// letter's colour, read from the canvas's --k-<letter> once per size.
const drawCanvas = `((s, g, r, p, x, l, L) => s && (
	r = devicePixelRatio || 1,
	el.width !== Math.round(el.clientWidth * r) && (el.width = Math.round(el.clientWidth * r), el.height = Math.round(el.clientHeight * r), el.__p = 0),
	p = el.__p || (el.__p = Object.fromEntries([...L].map(k => (el.style.color = 'var(--k-' + k + ')', [k, getComputedStyle(el).color])))),
	x = el.getContext('2d'),
	x.setTransform(el.width / 80, 0, 0, el.height / 50, 0, 0),
	x.clearRect(0, 0, 80, 50),
	[...s].forEach((k, i, _, q = k === '_' ? .24 : k < 'a' ? .65 : k > 'h' && k !== 'w' ? .45 : .86) => (
		k !== l && (x.fillStyle = p[l = k]),
		x.fillRect(i % 80 + (1 - q) / 2, (i / 80 | 0) + (1 - q) / 2, q, q)
	)),
	el.dataset.gen = g
))($_lifeb.c, $_lifeb.g, 0, 0, 0, '', '_abcdefghwABCDEFGHWijklmnopx')`

// Terminal's size, and its colours: 16 for the text, 8 for the ground.
const (
	termW, termH = 80, 24
	termChars    = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 .,:;-+=*#%/()[]{}|_~!?$@"
)

// terminal is a terminal's screen at generation g, as a renderer stress
// test draws it: each line a run of tokens of 1 to 12 characters, each
// token in its own colours, all of it new each frame. With runs, a token
// is one <span>; without, each character is.
func terminal(g int64, runs bool) string {
	rng := rand.New(rand.NewPCG(uint64(g), 0x7e57))
	var s strings.Builder
	s.WriteString(`<div id="life-board" class="term" aria-hidden="true" data-gen="` + strconv.FormatInt(g, 10) + `">`)
	for range termH {
		s.WriteString("<div>")
		for x := 0; x < termW; {
			n := min(1+rng.IntN(12), termW-x)
			class := "f" + strconv.Itoa(rng.IntN(16)) + " b" + strconv.Itoa(rng.IntN(8))
			if runs {
				s.WriteString(`<span class="` + class + `">`)
			}
			for range n {
				if !runs {
					s.WriteString(`<span class="` + class + `">`)
				}
				s.WriteByte(termChars[rng.IntN(len(termChars))])
				if !runs {
					s.WriteString("</span>")
				}
			}
			if runs {
				s.WriteString("</span>")
			}
			x += n
		}
		s.WriteString("</div>")
	}
	s.WriteString("</div>")
	return s.String()
}

// markupCSS lays out the other boards like the site's: the same dots,
// squares and colours.
const markupCSS = `
@scope ([data-demo="life"]) {
	.life__board--lean > p {
		color: var(--life-dot);
		background: radial-gradient(circle closest-side, currentColor calc(92% + 60% * var(--lith-notch)), transparent calc(100% + 60% * var(--lith-notch))) 50% 50% / 24% 24% no-repeat;
	}
	.life__board--lean > :is(.a, .b, .c, .d, .e, .f, .g, .h, .w) { color: var(--c); background-size: 86% 86%; }
	.life__board--lean > :is(.A, .B, .C, .D, .E, .F, .G, .H, .W) { color: color-mix(in oklab, var(--c) 67%, var(--life-dot)); background-size: 65% 65%; }
	.life__board--lean > :is(.i, .j, .k, .l, .m, .n, .o, .p, .x) { color: color-mix(in oklab, var(--c) 33%, var(--life-dot)); background-size: 45% 45%; }
	.life__board--lean > :is(.a, .A, .i), .life__board--svg :is(.a, .A, .i) { --c: var(--life-a); }
	.life__board--lean > :is(.b, .B, .j), .life__board--svg :is(.b, .B, .j) { --c: var(--life-b); }
	.life__board--lean > :is(.c, .C, .k), .life__board--svg :is(.c, .C, .k) { --c: var(--life-c); }
	.life__board--lean > :is(.d, .D, .l), .life__board--svg :is(.d, .D, .l) { --c: var(--life-d); }
	.life__board--lean > :is(.e, .E, .m), .life__board--svg :is(.e, .E, .m) { --c: var(--life-e); }
	.life__board--lean > :is(.f, .F, .n), .life__board--svg :is(.f, .F, .n) { --c: var(--life-f); }
	.life__board--lean > :is(.g, .G, .o), .life__board--svg :is(.g, .G, .o) { --c: var(--life-g); }
	.life__board--lean > :is(.h, .H, .p), .life__board--svg :is(.h, .H, .p) { --c: var(--life-h); }
	.life__board--lean > :is(.w, .W, .x), .life__board--svg :is(.w, .W, .x) { --c: var(--life-w); }

	.life__board--svg {
		display: block;
		& svg { display: block; inline-size: 100%; block-size: 100%; }
		& pattern rect { fill: var(--life-dot); }
		& path { fill: none; stroke-linecap: square; }
		& :is(.a, .b, .c, .d, .e, .f, .g, .h, .w) { stroke: var(--c); stroke-width: 0.86; }
		& :is(.A, .B, .C, .D, .E, .F, .G, .H, .W) { stroke: color-mix(in oklab, var(--c) 67%, var(--life-dot)); stroke-width: 0.65; }
		& :is(.i, .j, .k, .l, .m, .n, .o, .p, .x) { stroke: color-mix(in oklab, var(--c) 33%, var(--life-dot)); stroke-width: 0.45; }
	}

	.term {
		position: absolute;
		inset: 1px;
		display: block;
		overflow: hidden;
		/* 80 columns across the field, 24 lines fit its height. */
		font: calc(100cqw / 80 / 0.62)/1.25 var(--lith-font-code), monospace;
		white-space: pre;
		color: #ccc;
		background: #111;
		& .f0 { color: #000; } & .f1 { color: #c33; } & .f2 { color: #3c3; } & .f3 { color: #cc3; }
		& .f4 { color: #36c; } & .f5 { color: #c3c; } & .f6 { color: #3cc; } & .f7 { color: #ccc; }
		& .f8 { color: #666; } & .f9 { color: #f66; } & .f10 { color: #6f6; } & .f11 { color: #ff6; }
		& .f12 { color: #69f; } & .f13 { color: #f6f; } & .f14 { color: #6ff; } & .f15 { color: #fff; }
		& .b0 { background: #000; } & .b1 { background: #400; } & .b2 { background: #040; } & .b3 { background: #440; }
		& .b4 { background: #004; } & .b5 { background: #404; } & .b6 { background: #044; } & .b7 { background: #333; }
	}

	.life__board--canvas {
		display: block;
		inline-size: calc(100% - 2px);
		block-size: calc(100% - 2px);
		--k-_: var(--life-dot);
		--k-a: var(--life-a); --k-b: var(--life-b); --k-c: var(--life-c); --k-d: var(--life-d); --k-e: var(--life-e); --k-f: var(--life-f); --k-g: var(--life-g); --k-h: var(--life-h); --k-w: var(--life-w);
		--k-A: color-mix(in oklab, var(--life-a) 67%, var(--life-dot)); --k-B: color-mix(in oklab, var(--life-b) 67%, var(--life-dot)); --k-C: color-mix(in oklab, var(--life-c) 67%, var(--life-dot)); --k-D: color-mix(in oklab, var(--life-d) 67%, var(--life-dot)); --k-E: color-mix(in oklab, var(--life-e) 67%, var(--life-dot)); --k-F: color-mix(in oklab, var(--life-f) 67%, var(--life-dot)); --k-G: color-mix(in oklab, var(--life-g) 67%, var(--life-dot)); --k-H: color-mix(in oklab, var(--life-h) 67%, var(--life-dot)); --k-W: color-mix(in oklab, var(--life-w) 67%, var(--life-dot));
		--k-i: color-mix(in oklab, var(--life-a) 33%, var(--life-dot)); --k-j: color-mix(in oklab, var(--life-b) 33%, var(--life-dot)); --k-k: color-mix(in oklab, var(--life-c) 33%, var(--life-dot)); --k-l: color-mix(in oklab, var(--life-d) 33%, var(--life-dot)); --k-m: color-mix(in oklab, var(--life-e) 33%, var(--life-dot)); --k-n: color-mix(in oklab, var(--life-f) 33%, var(--life-dot)); --k-o: color-mix(in oklab, var(--life-g) 33%, var(--life-dot)); --k-p: color-mix(in oklab, var(--life-h) 33%, var(--life-dot)); --k-x: color-mix(in oklab, var(--life-w) 33%, var(--life-dot));
	}
}
`
