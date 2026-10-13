package checkboxes

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	mbits "math/bits"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// deps are what the page's views need at run time: the hub, for the
// watches and the counts, and the writer, for the Rewatch of a stream
// that opens. mount sets them; a process runs one site.
type deps struct {
	hub Hub
	w   *writer.Writer
}

var rt atomic.Pointer[deps]

// renderPage draws the demo into the GET document: the numbers, and the
// grid at the top, its tiles read in the document's transaction. A new
// tab has stored no window yet.
func renderPage(ctx context.Context, tx *sql.Tx, v demo.Viewer) (templ.Component, error) {
	q := demo.Querier(ctx, tx)
	w, err := window(ctx, q, v)
	if err != nil {
		return nil, err
	}
	tiles, err := chunks(ctx, q, w)
	if err != nil {
		return nil, err
	}
	s, err := stats(ctx, q, time.Now())
	if err != nil {
		return nil, err
	}
	if d := rt.Load(); d != nil {
		s.Here = d.hub.Count(StatsKey) + 1 // and this page, whose stream is on its way
	}
	return Stage(w, tiles, s), nil
}

// pageViews are what a tab's stream starts with: the tab's own view, the
// list at its window, and the numbers, the same for everyone. The chunks
// come a moment later, from a Rewatch on the writer, so that the stream
// watches its window's chunks whatever windows the tab stores meanwhile.
func pageViews(v demo.Viewer) []live.View {
	d := rt.Load()
	var h Hub
	if d != nil {
		h = d.hub
		time.AfterFunc(rewatchAfter, func() {
			// Refused while the server stops: the next process watches
			// the window when the page comes back.
			d.w.Send(Rewatch{Viewer: v, Hub: d.hub})
		})
	}
	return []live.View{boardView(v), statsView(h)}
}

// boardView is the tab's own view: the list at the tab's window, its slots
// in the window's order. The slots go out empty: data-ignore-morph keeps
// the tiles the chunks' frames put in them, and the morph moves them by
// their ids.
func boardView(v demo.Viewer) live.View {
	key := demo.TabTopic(Slug, v.Tab)
	return live.View{
		Key:    key,
		Topics: []string{key},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			w, err := window(ctx, demo.Querier(ctx, tx), v)
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, Board(w, nil))
		},
	}
}

// chunkView is chunk n's tile, one view for every tab that shows it. Its
// frame carries that tile and nothing else, into the slot every page
// keeps for it. The tile says how many tabs watch it, so it renders again
// when one comes or goes (Presence).
func chunkView(h Hub, n int) live.View {
	key := ChunkTopic(n)
	return live.View{
		Key:      key,
		Topics:   []string{key},
		Presence: true,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			bits, err := chunk(ctx, demo.Querier(ctx, tx), n)
			if err != nil {
				return nil, err
			}
			return tileFrame(n, bits, h.Count(key)), nil
		},
	}
}

// statsView is the numbers above the grid, the same for everyone. It
// renders once a second, like a clock, and when a tab comes or goes: the
// rate counts whole seconds, and a flip goes to every tab on the page in
// the numbers, so they follow the flips a second at a time and the tiles
// tick by tick.
func statsView(h Hub) live.View {
	return live.View{
		Key:      StatsKey,
		Every:    time.Second,
		Presence: true,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			s, err := stats(ctx, demo.Querier(ctx, tx), time.Now())
			if err != nil {
				return nil, err
			}
			if h != nil {
				s.Here = h.Count(StatsKey)
			}
			return demo.Patch(ctx, StatsBar(s))
		},
	}
}

// tileBufs are buffers for tileFrame: a tile is written into one, and the
// event copies it out.
var tileBufs = sync.Pool{New: func() any { return bytes.NewBuffer(make([]byte, 0, 10<<10)) }}

// tileFrame is the event that morphs chunk n's tile into its slot.
func tileFrame(n int, bits []byte, watching int) []byte {
	b := tileBufs.Get().(*bytes.Buffer)
	defer tileBufs.Put(b)
	b.Reset()
	writeTile(b, n, bits, watching)
	return sse.PatchElements(b.Bytes(), sse.Patch{Selector: "#" + slotID(slot(n)), Mode: sse.Inner})
}

// writeTile writes chunk n's tile: the number of its first box, how many
// tabs watch it when more than one does, how many of its boxes are
// checked, and a checkbox for each box, named by its number and checked
// where its bit is set. The page counts a box's number from the tile's
// data-chunk and the box's place in it, so a box carries no more than
// that.
func writeTile(b *bytes.Buffer, n int, bits []byte, watching int) {
	first, on := n*ChunkSize+1, 0
	for _, c := range bits {
		on += mbits.OnesCount8(c)
	}
	b.WriteString(`<div id="checkboxes-tile-`)
	b.WriteString(strconv.Itoa(n))
	b.WriteString(`" class="cb-tile" role="listitem" aria-label="Boxes `)
	b.Write(appendGrouped(b.AvailableBuffer(), first))
	b.WriteString(` to `)
	b.Write(appendGrouped(b.AvailableBuffer(), first+size(n)-1))
	b.WriteString(`, `)
	b.WriteString(strconv.Itoa(on))
	b.WriteString(` checked" data-chunk="`)
	b.WriteString(strconv.Itoa(n))
	b.WriteString(`" data-init="$_checkboxes.moved++"><p class="cb-tile__head" aria-hidden="true"><span>#`)
	b.Write(appendGrouped(b.AvailableBuffer(), first))
	b.WriteString(`</span>`)
	if watching > 1 {
		b.WriteString(`<span class="cb-tile__eyes">`)
		b.WriteString(strconv.Itoa(watching))
		b.WriteString(` watching</span>`)
	}
	if on > 0 {
		b.WriteString(`<span class="cb-tile__on">`)
		b.WriteString(strconv.Itoa(on))
		b.WriteString(` checked</span>`)
	}
	b.WriteString(`</p><div class="cb-boxes">`)
	for i := range size(n) {
		b.WriteString(`<input type=checkbox title=`)
		b.Write(appendGrouped(b.AvailableBuffer(), first+i))
		if isSet(bits, i) {
			b.WriteString(` checked`)
		}
		b.WriteByte('>')
	}
	b.WriteString(`</div></div>`)
}

// tile is a tile for templ: the GET document's.
func tile(n int, bits []byte, watching int) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		var b bytes.Buffer
		writeTile(&b, n, bits, watching)
		_, err := w.Write(b.Bytes())
		return err
	})
}

func slotID(s int) string { return "checkboxes-slot-" + strconv.Itoa(s) }

// watchers is how many tabs will watch chunk n once the page asking for
// it does: those that do, and this one.
func watchers(n int) int {
	if d := rt.Load(); d != nil {
		return d.hub.Count(ChunkTopic(n)) + 1
	}
	return 1
}
