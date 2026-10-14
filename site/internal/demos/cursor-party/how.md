## The moves

The floor follows the pointer on the window, so the hero's links and buttons, which sit above the floor, take no move away. Datastar's throttle lets a move through every 67 ms and drops the ones in between, so a debounce sends the place where the pointer comes to rest. A tap or a click sends its place too.

```go title=expr.go
func floor() templ.Attributes {
	return templ.Attributes{
		"data-on:pointermove__window__throttle." + ms(Throttle): move,
		"data-on:pointermove__window__debounce." + ms(Rest):     move,
		"data-on:pointerdown": move,
		"data-init":           anchorHomes(),
	}
}
```

The floor's place is not the same on every screen: a phone stacks the hero's words that a desktop lays out side by side. So a move names an anchor, the nearest of the boxes the hero marks with `data-cp-anchor` (a line of the acrostic, the monolith, the lede, the buttons), and gives the pointer's place in fractions of that box. The floor's `data-init` measures the anchors on this page, hands their boxes to CSS as custom properties on the floor, which the stream never patches, and measures again when the floor or an anchor changes size. A page without an anchor, like this one, puts it where the desktop hero has it.

```go title=expr.go
const measure = `const names = NAMES, homes = HOMES;
const find = (n) => document.querySelector('[data-cp-anchor="' + n + '"]');
const run = () => {
	const f = el.getBoundingClientRect(), at = {};
	for (const n of names) {
		const a = find(n), r = a && a.getBoundingClientRect(), h = homes[n];
		const b = r && r.width && r.height ? [r.left - f.left, r.top - f.top, r.width, r.height] : [h[0] * f.width, h[1] * f.height, h[2] * f.width, h[3] * f.height];
		at[n] = b;
		['x', 'y', 'w', 'h'].forEach((k, i) => el.style.setProperty('--cp-' + n + '-' + k, b[i] + 'px'));
	}
	el.cpAnchors = at;
};
run();
const ro = new ResizeObserver(run);
ro.observe(el);
for (const n of names) { const a = find(n); a && ro.observe(a) }`
```

A move carries that anchor, the place beside it, and the next number of `$_cursorParty.seq`. Moves go out side by side: none cancels the one before, and none is retried.

```go title=expr.go
const move = `const f = el.getBoundingClientRect(), px = evt.clientX - f.left, py = evt.clientY - f.top, at = el.cpAnchors;
if (evt.isPrimary && at && px >= 0 && py >= 0 && px <= f.width && py <= f.height) {
	let best = '', d = Infinity;
	for (const n in at) {
		const b = at[n], dx = Math.max(b[0] - px, 0, px - b[0] - b[2]), dy = Math.max(b[1] - py, 0, py - b[1] - b[3]);
		if (dx * dx + dy * dy < d) { d = dx * dx + dy * dy; best = n }
	}
	const b = at[best], r = (v) => Math.max(-20, Math.min(20, Math.round(v * 1e4) / 1e4));
	@post('/cmd/cursor-party/move', {
		payload: {tab: $_tab, a: best, x: r((px - b[0]) / b[2]), y: r((py - b[1]) / b[3]), seq: ++$_cursorParty.seq},
		requestCancellation: 'disabled', retry: 'never', retryMaxCount: 0,
	}).catch(() => {})
}`
```

## The command

`Move` updates the tab's row unless the row holds a higher number, so a move that another overtook on the way changes nothing. The tab's first move inserts the row. The handler queues the command with `k.W.Send` and answers 204 at once.

```go title=commands.go
func (c Move) Apply(tx *writer.Tx) error {
	id, name, hue := Identity(c.Viewer.Tab)
	at := c.At.UnixMilli()
	if c.A == "" {
		c.A = floorAnchor
	}
	res, err := tx.Exec(`UPDATE cursor_party_cursors SET a = ?, x = ?, y = ?, seq = ?, at = ? WHERE id = ? AND sid = ? AND seq < ?`,
		c.A, c.X, c.Y, c.Seq, at, id, c.Viewer.Session, c.Seq)
	if err != nil {
		return err
	}
	moved, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if moved == 0 {
		// The tab's first move, or one that a newer move overtook, or a tab
		// id from another session: only the first inserts a row.
		res, err := tx.Exec(`INSERT INTO cursor_party_cursors (id, sid, name, hue, a, x, y, seq, born, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO NOTHING`, id, c.Viewer.Session, name, hue, c.A, c.X, c.Y, c.Seq, at, at)
		if err != nil {
			return err
		}
		if moved, err = res.RowsAffected(); err != nil || moved == 0 {
			return err
		}
		if err := makeRoom(tx, at); err != nil {
			return err
		}
	}
	// The counter's moves of each second.
	if _, err := tx.Exec(`INSERT INTO cursor_party_seconds (second, moves) VALUES (?, 1)
		ON CONFLICT (second) DO UPDATE SET moves = moves + 1`, c.At.Unix()); err != nil {
		return err
	}
	tx.Touch(Topic)
	return nil
}
```

A new row makes room as well: the rows of tabs quiet for two minutes go, and the oldest beyond a thousand, so the table stays small between two sweeps whatever tab ids the requests make up.

## The layer

The layer is a shared view. Its key holds nothing of the viewer, so the hub renders it once a tick for every tab at the party, on both pages. `Every` renders it once a second while nobody moves, so that quiet cursors fade and leave.

```go title=live.go
func cursorsView() live.View {
	return live.View{
		Key:    cursorsKey,
		Topics: []string{Topic},
		Every:  time.Second,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			now := time.Now()
			list, err := cursors(ctx, demo.Querier(ctx, tx), now)
			if err != nil {
				return nil, err
			}
			return sse.PatchElements(layer(list, now)), nil
		},
	}
}
```

The 64 newest cursors come in the order of their ids. A cursor keeps its place in the layer from one frame to the next, so a morph changes its style attribute and never moves the element, which would restart its transitions.

## Your own cursor

The frame is the same for every tab, so it holds your cursor too. The page hides it with a style it rendered once, by the id that the server makes from your tab id.

```go title=layer.go
func selfStyle(tab string) string {
	id, _, _ := Identity(tab)
	return `#cursor-party-` + id + `{display:none}`
}
```

The id is 12 hex digits of a SHA-256 of the tab id. Every page sees it, while the tab id stays with its own page: a stream that opens with another tab's id replaces that tab's stream.

## The motion

A frame changes `--x` and `--y` of the cursors that moved, in percent of their anchor's box (`data-a` picks its custom properties); the place in pixels is the box's corner plus that share of its size, kept on the floor with `clamp()`. Each part of a cursor eases there on its own. The name tag, which carries the arrow, takes 120 ms; the two sparks of the trail take 180 and 260 ms, so in motion they fall behind along the path, and at rest they hide under the arrow.

```css title=cursor-party.css
&::before,
&::after {
	content: "";
	position: absolute;
	inset: 0 auto auto 0;
	z-index: 0;
	inline-size: 6px;
	block-size: 6px;
	border-radius: calc(50% * (1 - var(--lith-notch)));
	background: var(--_c);
	box-shadow: 0 0 10px 1px color-mix(in oklch, var(--_c) 60%, transparent);
	opacity: 0.6;
	translate: calc(var(--_x) + 3px) calc(var(--_y) + 9px);
	transition: translate 180ms linear;
}

&::after {
	inline-size: 4px;
	block-size: 4px;
	opacity: 0.35;
	translate: calc(var(--_x) + 4px) calc(var(--_y) + 10px);
	transition-duration: 260ms;
}
```

With reduced motion there is no trail, and each frame puts every cursor in its place at once.

## The counter

The tabs here are the streams that watch the party, as the hub counts them; the counter's view has `Presence`, so it renders again when a tab comes or goes. Beside them are the moves of the last whole second, and Starbase's `sb-sparkline` with the moves of each of the last 30 seconds, one row a second in `cursor_party_seconds`. The second under way is left out, so the counter's frame changes once a second at most.

```go title=views.templ
<sb-sparkline values={ spark(history) } length={ strconv.Itoa(History) } scale="fixed" min="0" max={ sparkTop(history) } aria-hidden="true"></sb-sparkline>
```

The numbers are integers registered with `@property`, which CSS draws as counters, so when a frame changes one it rolls to the new value.

```css title=cursor-party.css
@property --cursor-party-rate {
	syntax: "<integer>";
	inherits: true;
	initial-value: 0;
}
```
