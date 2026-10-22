package checkboxes

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	mbits "math/bits"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// renderPage draws the demo into the GET document: the numbers, and the
// grid at the tab's window, its tiles read in the document's transaction.
// A new tab has stored no window: the top.
func renderPage(ctx context.Context, tx *sql.Tx, v demo.Viewer) (templ.Component, error) {
	q := demo.Querier(ctx, tx)
	w, err := window(ctx, q, v)
	if err != nil {
		return nil, err
	}
	bits, err := chunks(ctx, q, w)
	if err != nil {
		return nil, err
	}
	tiles := make([]shown, len(bits))
	for i, b := range bits {
		// This page's stream is on its way: one more on every tile.
		tiles[i] = shown{bits: b, watching: demo.Watching(ctx, ChunkTopic(w.Start+i)) + 1}
	}
	s, err := stats(ctx, q, time.Now())
	if err != nil {
		return nil, err
	}
	s.Here = demo.Watching(ctx, StatsKey) + 1
	return Stage(w, tiles, s), nil
}

// shown is a tile as the GET document draws it.
type shown struct {
	bits     []byte
	watching int
}

// pageViews are what a tab's stream watches from the start: the tab's own
// view, the list at its window, and the numbers, the same for everyone.
// The chunks come when the page answers the list's first frame with its
// window (boardView).
func pageViews(v demo.Viewer) []live.View {
	return []live.View{boardView(v), statsView(), clicksView(v)}
}

// clicksView acks the tab's clicks: the newest op the server took, which
// ends the pending state of that box and the older ones, and that the
// stream the page opened has brought it (demo.Pending).
func clicksView(v demo.Viewer) live.View {
	key := demo.TabTopic(ClicksKey, v.Tab)
	return live.View{
		Key:    key,
		Topics: []string{key},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			var k Clicks
			if err := demo.LoadTab(ctx, demo.Querier(ctx, tx), ClicksKey, v, &k); err != nil {
				return nil, err
			}
			return demo.Signals(map[string]any{"_checkboxes": map[string]any{"ack": k.Op, "synced": true}})
		},
	}
}

// boardView is the tab's own view: the list at the tab's window, its slots
// in the window's order. The slots go out empty: data-ignore-morph keeps
// the tiles the chunks' frames put in them, and the morph moves them by
// their ids.
//
// A stream sends the view's first form when it opens, the next one from
// then on. The first also sets $_checkboxes.hello, and the page answers
// with its window (SetWindow), which watches the window's chunks: so a
// stream that opens, the first one or one after a deploy or a dropped
// connection, watches the chunks its page shows, and the request that
// says so comes after the stream is there.
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
			board, err := demo.Patch(ctx, Board(w, nil))
			if err != nil {
				return nil, err
			}
			return demo.Frame{First: append(slices.Clip(board), hello...), Next: board}, nil
		},
	}
}

// hello asks the page for its window.
var hello = sse.PatchSignals([]byte(`{"_checkboxes":{"hello":true}}`), false)

// chunkView is chunk n's tile, one view for every tab that shows it. Its
// frame carries that tile and nothing else, into the slot every page
// keeps for it. The tile says how many tabs watch it, so it renders again
// when one comes or goes (Presence).
func chunkView(n int) live.View {
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
			return tileFrame(n, bits, demo.Watching(ctx, key)), nil
		},
	}
}

// statsView is the numbers above the grid, the same for everyone. It
// renders once a second, like a clock, and when a tab comes or goes: the
// rate counts whole seconds, and a flip goes to every tab on the page in
// the numbers, so they follow the flips a second at a time and the tiles
// tick by tick.
func statsView() live.View {
	return live.View{
		Key:      StatsKey,
		Every:    time.Second,
		Presence: true,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			s, err := stats(ctx, demo.Querier(ctx, tx), time.Now())
			if err != nil {
				return nil, err
			}
			s.Here = demo.Watching(ctx, StatsKey)
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
