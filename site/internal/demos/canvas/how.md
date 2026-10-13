## The board is one row

A 64 by 64 board in 16 colours is 4,096 hex digits: the `cells` attribute of Starbase's `sb-pixel-board`, which draws them on a canvas. SQLite keeps half of that, one BLOB of 2,048 bytes with two cells to a byte, so the attribute is the BLOB in hex. When each cell was last painted lives beside it in `canvas_painted`, a row per cell that is not blank, which the sweep reads by an index on the minute.

The board's view and every page load need the attribute. It is encoded once per version of the board; the renders after that take the string from memory, keyed by the version they read in their own transaction.

```go title=board.go
func cellsOf(ctx context.Context, q sqlite.Querier) (string, *instance, error) {
	var epoch, version int64
	if err := q.QueryRowContext(ctx, `SELECT epoch, version FROM canvas_board WHERE id = 1`).Scan(&epoch, &version); err != nil {
		return "", nil, err
	}
	in := instanceOf(epoch)
	if e := in.encoded.Load(); e != nil && e.version == version {
		return e.cells, in, nil
	}
	var cells []byte
	if err := q.QueryRowContext(ctx, `SELECT cells FROM canvas_board WHERE id = 1`).Scan(&cells); err != nil {
		return "", nil, err
	}
	e := &encoded{version: version, cells: hex.EncodeToString(cells)}
	for {
		old := in.encoded.Load()
		if old != nil && old.version >= version || in.encoded.CompareAndSwap(old, e) {
			break // a newer version stays
		}
	}
	return e.cells, in, nil
}
```

## The command

A stroke is a run of `Paint` commands, one per batch of cells. The writer applies each in a savepoint of the batch's transaction: it reads the 2,048 bytes, sets the cells' nibbles, writes them back, and starts the cells' day with one statement however many cells there are. A paint that changes no colour, a painter going over their own pixels, renews the day and leaves the board's version alone.

```go title=commands.go
func paintCells(tx *writer.Tx, list []int, color int, at time.Time) (bool, error) {
	var cells []byte
	if err := tx.QueryRow(`SELECT cells FROM canvas_board WHERE id = 1`).Scan(&cells); err != nil {
		return false, err
	}
	b, changed := board(cells), 0
	for _, i := range list {
		if b.color(i) != color {
			b.setColor(i, color)
			changed = 1
		}
	}
	if _, err := tx.Exec(`UPDATE canvas_board SET version = version + ?, pixels = pixels + ?, cells = ? WHERE id = 1`,
		changed, len(list), cells); err != nil {
		return false, err
	}
	var err error
	if color == 0 { // a blank cell has no day to live
		_, err = tx.Exec(`DELETE FROM canvas_painted WHERE cell IN (SELECT value FROM json_each(?))`, cellList(list))
	} else {
		_, err = tx.Exec(`INSERT INTO canvas_painted (cell, minute) SELECT value, ? FROM json_each(?) WHERE true
			ON CONFLICT (cell) DO UPDATE SET minute = excluded.minute`, minute(at), cellList(list))
	}
	return changed == 1, err
}
```

`Apply` then touches `canvas:cells` if a colour changed and `canvas:paints` always, records the paint's colour as the painter's, and adds the pixels to the painter's second of the last minute.

## The action

The kit limits the requests of every action. Painting needs a limit on pixels too, since one request carries 1 to 60 of them: a token bucket per session, and one per address with four times as much, checked before the command reaches the writer.

```go title=routes.go
	bySession := edge.NewLimiter(pixelRate, pixelBurst)
	byAddr := edge.NewLimiter(4*pixelRate, 4*pixelBurst)
	k.Command("paint", demo.Rate{Every: paintEvery, Burst: paintBurst}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tab   string
			Color int
			Cells []int
		}
		if !demo.Decode(w, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		slices.Sort(req.Cells)
		now := time.Now()
		cmd := Paint{Viewer: v, Color: req.Color, Cells: slices.Compact(req.Cells), At: now}
		if err := cmd.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if n := len(cmd.Cells); !byAddr.Allow(edge.ClientKey(r), n, now) || !bySession.Allow(v.Session, n, now) {
			http.Error(w, "Painting too fast: 20 pixels a second each.", http.StatusTooManyRequests)
			return
		}
		k.Do(w, r, cmd)
	})
```

## The views

The board is one view for every painter. It renders once in a tick in which a cell changed, however many paints that tick took, and every stream gets the same frame.

```go title=live.go
func boardView() live.View {
	return live.View{
		Key:    boardKey,
		Topics: []string{TopicCells},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			cells, _, err := cellsOf(ctx, demo.Querier(ctx, tx))
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, Board(cells))
		},
	}
}
```

The frame morphs the element by its id, and only `cells` differs from the last one. The palette and the brush are the page's own, set from its signals: `data-preserve-attr="palette color"` keeps them through the morph.

The painters here are the hub's knowledge, not a table: `hub.Watchers` lists the ids of the streams that watch a view, and `Presence` renders the list again when one comes or goes. A tab id never appears in it. Whoever knows a tab's id could open a stream in its name and end the tab's own, so each painter is shown by a hash of it, which also draws their face.

```go title=live.go
func hereView() live.View {
	return live.View{
		Key:      hereKey,
		Topics:   []string{TopicPainters},
		Presence: true,
		Every:    2 * time.Second,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			q := demo.Querier(ctx, tx)
			in, err := instanceIn(ctx, q)
			if err != nil {
				return nil, err
			}
			list, err := painters(ctx, q, in.watchers(hereKey), time.Now())
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, Here(list))
		},
	}
}
```

Every two seconds it renders again for the counts of the last minute; a frame equal to the one before goes nowhere. Which face is yours the shared view cannot say: the document renders a style for your tab's hash, which rings it.

## The page

`sb-pixel-board` batches a stroke into an `sb-paint` event every 80 ms. Datastar would cancel a request still under way when the next one goes to the same URL, so the post turns that off: on a slow connection a stroke would lose pixels otherwise.

```go title=expr.go
const paint = `@post('/cmd/canvas/paint', {payload: {tab: $_tab, color: evt.detail.color, cells: evt.detail.cells}, requestCancellation: 'disabled'})`
```

The pixels you paint show faded until the board's frame brings them back. If no frame confirms them within three seconds, because the server refused the request, the component takes them back.

The component paints with sixteen hex colours, and the site's colours are tokens with a light and a dark value. The palette is sixteen tokens in the demo's CSS: the neutrals turn over with the theme, like ink on paper and chalk on slate, so a drawing keeps its contrast; the hues keep their colour, but the palest three take a darker step on paper.

```css title=canvas.css
	:scope {
		--canvas-c0: var(--lith-surface-inset);
		--canvas-c1: light-dark(var(--lith-stone-12), var(--lith-stone-1));
		--canvas-c2: light-dark(var(--lith-stone-9), var(--lith-stone-3));
		--canvas-c3: light-dark(var(--lith-stone-7), var(--lith-stone-5));
		--canvas-c4: light-dark(var(--lith-stone-5), var(--lith-stone-7));
		--canvas-c5: light-dark(var(--lith-stone-3), var(--lith-stone-9));
		--canvas-c6: light-dark(var(--lith-ember-3), var(--lith-art-seam-core));
		--canvas-c7: var(--lith-art-seam);
		--canvas-c8: var(--lith-art-seam-edge);
		--canvas-c9: var(--lith-ember-9);
		--canvas-c10: light-dark(var(--lith-ember-6), var(--lith-ember-4));
		--canvas-c11: var(--lith-code-number);
		--canvas-c12: var(--lith-sage-light);
		--canvas-c13: var(--lith-green-dark);
		--canvas-c14: var(--lith-red-light);
		--canvas-c15: var(--lith-red-dark);
	}
```

The page turns them into hex: a hidden probe takes each token as its colour, and a 1 by 1 canvas turns whatever the browser computed into sRGB. It reads them again when the theme changes, and the board stays hidden until it has them.

```go title=expr.go
const start = `
const probe = el.querySelector('.cv__probe');
const pen = document.createElement('canvas').getContext('2d', {willReadFrequently: true});
const read = () => {
	$_canvas.palette = JSON.stringify(Array.from({length: 16}, (_, i) => {
		probe.style.color = 'var(--canvas-c' + i + ')';
		pen.fillStyle = getComputedStyle(probe).color;
		pen.fillRect(0, 0, 1, 1);
		const [r, g, b] = pen.getImageData(0, 0, 1, 1).data;
		return '#' + ((1 << 24) | (r << 16) | (g << 8) | b).toString(16).slice(1);
	}));
};
read();
new MutationObserver(read).observe(document.documentElement, {attributeFilter: ['data-lith-theme']});
matchMedia('(prefers-color-scheme: dark)').addEventListener('change', read);
@post('/cmd/canvas/pick', {payload: {tab: $_tab, color: $_canvas.color}})`
```

## A day per pixel

A canvas that never empties fills with noise until nobody can draw on it. Here each pixel lasts a day from its last paint. Every five minutes the sweep paints the cells whose day is over in the background: one indexed range of `canvas_painted`, one write of the board, one frame for everyone.

```go title=commands.go
func fade(tx *writer.Tx, now time.Time) error {
	limit := minute(now.Add(-Lifetime))
	rows, err := tx.Query(`SELECT cell FROM canvas_painted WHERE minute <= ?`, limit)
	if err != nil {
		return err
	}
	var faded []int
	for rows.Next() {
		var cell int
		if err := rows.Scan(&cell); err != nil {
			rows.Close()
			return err
		}
		faded = append(faded, cell)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil || len(faded) == 0 {
		return err
	}
	var cells []byte
	if err := tx.QueryRow(`SELECT cells FROM canvas_board WHERE id = 1`).Scan(&cells); err != nil {
		return err
	}
	for _, i := range faded {
		board(cells).setColor(i, 0)
	}
	if _, err := tx.Exec(`UPDATE canvas_board SET version = version + 1, cells = ? WHERE id = 1`, cells); err != nil {
		return err
	}
	_, err = tx.Exec(`DELETE FROM canvas_painted WHERE minute <= ?`, limit)
	tx.Touch(TopicCells, TopicPaints)
	return err
}
```

Every table stays small: the board is one row, `canvas_painted` holds at most a row per cell, `canvas_recent` at most 60 rows per painter, and painters go a day after they were last seen, the oldest first beyond 5,000.

## Measured

On an Intel i5-13500 under Linux, in October 2026, with other work on the machine:

- The writer takes about 30,000 paints a second from 100 painters at once, 50 paints to a transaction: 60,000 pixels a second in the two-pixel batches of a stroke at the limit, 500,000 in batches of 60. The limits let 300 streams send 6,000.
- The board's frame is 4.8 KB of HTML. After a one-pixel paint the board and the pulse together cost 44 bytes on the wire with zstd and 42 with brotli; a painter's first frames, 1.3 KB.
- 100 painters at the limit, 2,000 pixels a second together, on the built binary with the site's memory limit (192 MiB): no request refused or failed, the writer busy 9 to 20% of the time, the process at 0.4 to 0.75 cores, and 4 KB a second to each painter, about 105 bytes a frame. The live heap is about 95 MiB, 83 MiB of it the 100 streams' zstd encoders; the process stays near the limit, since the collector lets the heap use the room up to it.
