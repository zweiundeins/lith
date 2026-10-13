## The boxes

A box is a bit. The bits sit in 3,907 rows of 256, each a 32-byte BLOB under an integer key, the chunk's number: a click reads one short row by its key and writes it back. A row per box would be a million rows, and a million keys to look up when a tile renders.

`SetBox` carries the state the box should take, not a toggle. Two people who tick the same clear box at once get one checked box, and a request the network sends twice changes nothing the second time, so the command needs no op id.

```go title=commands.go
func (c SetBox) Apply(tx *writer.Tx) error {
	n, i := c.Box/ChunkSize, c.Box%ChunkSize
	var bits []byte
	if err := tx.QueryRow(`SELECT bits FROM checkboxes_chunks WHERE id = ?`, n).Scan(&bits); err != nil {
		return err
	}
	if isSet(bits, i) == c.Checked {
		return nil // so already: nothing changes, nothing renders
	}
	bits[i>>3] ^= 1 << (i & 7)
```

After the write it counts the flip in a row of totals and a row for the second, and touches the chunk's topic, nothing else.

## A view per chunk

Each chunk is a view of its own, keyed by its topic. Every tab that shows the chunk shares it: a flip renders it once, and the same frame goes to each of them. The frame holds the chunk's tile and no more, about 8.6 KB of HTML, so a flip costs the tabs that show that tile and leaves the rest alone.

```go title=live.go
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
```

`Presence` renders the tile again when a tab starts or stops watching it, so it can say how many tabs show it. Open the demo in two windows: the tiles both show light up with "2 watching", and scrolling one window away puts them back.

## Watching what the tab shows

`sb-virtual-scroll` asks for a window as you scroll (`sb-window`, with an offset and a count). The page posts it as a command, rate-limited like a click, and the command does two things: it stores the window in the tab's row of `site_tabs`, and after the commit it moves the tab's stream to the window's twelve chunks.

```go title=commands.go
func (c SetWindow) Apply(tx *writer.Tx) error {
	old, err := window(tx.Context(), tx, c.Viewer)
	if err != nil {
		return err
	}
	if c.Window != old {
		if err := demo.SaveTab(tx, Slug, c.Viewer, c.Window, c.At); err != nil {
			return err
		}
	}
	tx.AfterCommit(func() { follow(c.Hub, c.Viewer.Tab, old, c.Window) })
	return nil
}
```

`follow` calls `hub.Unwatch` for each chunk the tab left and `hub.Watch` for each chunk of the new window. A chunk another tab watches sends its newest frame at once; one nobody watched renders in the next tick.

Why a command, and not the stream's own request with the window in its URL: the window is the tab's state, so it outlives a deploy in SQLite; the watches change on the writer's goroutine, in the order the windows were stored, so two windows sent in quick succession can never leave the stream on the older one; and the stream stays open while you scroll, keeping its zstd history. A stream opens with the tab's own view and the numbers only. A moment after it opens, a `Rewatch` command reads the tab's window and watches its chunks, again on the writer's goroutine, so a page that comes back after a deploy or a dropped connection gets the chunks it shows.

## Slots

The list's items are twelve slots, and chunk `n` always sits in slot `n % 12`. Its frame is addressed to that slot, the same on every page, so it can be shared, and it always finds the slot even when it arrives before the list has moved: `Watch` sends a frame at once, while the tab's view with the new order renders in the next tick.

```templ title=views.templ
for n := w.Start; n < w.End(); n++ {
	<div id={ slotID(slot(n)) } class="cb-slot" data-ignore-morph>
		if tiles != nil {
			@tile(n, tiles[n-w.Start], watchers(n))
		}
	</div>
}
```

The tab's view sends the slots empty, in the window's order. The morph moves them by their ids, and `data-ignore-morph` keeps the tiles inside as they are. A slot the window leaves empty, where the grid ends, waits in a hidden park for its next chunk.

## A box that waits

The browser toggles a checkbox before the click handler runs. The handler puts it back, so the box keeps showing what the server said, marks it indeterminate, and posts the state it should take:

```go title=expr.go
evt.preventDefault();
const want = b.checked, sent = (b.cbSent || 0) + 1;
b.cbSent = sent;
$_checkboxes.problem = '';
// Pending: indeterminate, unless the frame came first. Five seconds
// without one (someone set it back in the same tick) end it too.
setTimeout(() => b.checked !== want && b.cbSent === sent && (b.indeterminate = true));
setTimeout(() => b.cbSent === sent && (b.indeterminate = false), 5000);
@post('/cmd/checkboxes/set', {payload: {box: n, checked: want}, requestCancellation: 'disabled'})
```

`:indeterminate` blinks the box's edge. When the tile's frame flips the box, Datastar's morph sets its `checked` and fires `datastar-prop-change` on it, and the page ends the wait there. The morph never touches `indeterminate`, so a frame that changes another box in the tile leaves the wait alone.

## The numbers

Boxes checked, toggles per second and people here render once a second, like a clock, and when a tab comes or goes. They go to every tab on the page, so a render each tick would be a frame to every tab for every tick anyone clicks; once a second they cost a twentieth of that, and the tiles still move tick by tick.

## What it costs

Measured on the i5-13500 the site's numbers come from, in process with the site's memory limit (GOMEMLIMIT 192 MiB, so the streams' encoders share 96 MiB), on 13 October 2026, with `LITH_SITE_MEASURE=1 go test ./internal/app -run TestMeasureCheckboxes -v`:

- The writer takes 14,500 to 18,600 clicks a second from one person waiting for each commit, 46,000 to 62,000 from 128 at once and 62,000 to 83,000 from 512, about 450 to a transaction. The site lets one address send 20 a second.
- A tile is 8.6 KB of HTML. On a zstd stream a whole window, twelve tiles with the list and the numbers, is 8 KB, and a flip about 50 bytes.
- 100 tabs at 100 places watch 1,200 chunk views: 113 MiB of heap, 83 MiB of it the streams' encoders and about 10 MiB the tiles the hub holds, 1 MiB a tab.
- With those tabs open, 50 clicks a second take 2% of a core and 900 take 20%, with no tick over 50 ms. 48 windows a second, every tab moving every two seconds, take 8%.
- 100 tabs at the same place share twelve views; 50 clicks a second in view of all of them take 17% of a core and 4.4 KiB a second for each tab.
