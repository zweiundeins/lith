## The moves

The floor follows the pointer on the window, so the hero's links and buttons, which sit above the floor, take no move away. Datastar's throttle lets a move through every 67 ms and drops the ones in between, so a debounce sends the place where the pointer comes to rest. A tap or a click sends its place too.

```go title=expr.go
func floor() templ.Attributes {
	return templ.Attributes{
		"data-on:pointermove__window__throttle." + ms(Throttle): move,
		"data-on:pointermove__window__debounce." + ms(Rest):     move,
		"data-on:pointerdown": move,
	}
}
```

A move is the pointer's place in fractions of the floor, the same on any screen, and the next number of `$_cursorParty.seq`. Moves go out side by side: none cancels the one before, and none is retried.

```go title=expr.go
const move = `const r = el.getBoundingClientRect(), x = (evt.clientX - r.left) / r.width, y = (evt.clientY - r.top) / r.height;
evt.isPrimary && x >= 0 && x <= 1 && y >= 0 && y <= 1 && @post('/cmd/cursor-party/move', {
	payload: {tab: $_tab, x: Math.round(x * 1e4) / 1e4, y: Math.round(y * 1e4) / 1e4, seq: ++$_cursorParty.seq},
	requestCancellation: 'disabled', retry: 'never', retryMaxCount: 0,
}).catch(() => {})`
```

## The command

`Move` updates the tab's row unless the row holds a higher number, so a move that another overtook on the way changes nothing. The tab's first move inserts the row. The handler queues the command with `k.W.Send` and answers 204 at once.

```go title=commands.go
func (c Move) Apply(tx *writer.Tx) error {
	id, name, hue := Identity(c.Viewer.Tab)
	at := c.At.UnixMilli()
	res, err := tx.Exec(`UPDATE cursor_party_cursors SET x = ?, y = ?, seq = ?, at = ? WHERE id = ? AND sid = ? AND seq < ?`,
		c.X, c.Y, c.Seq, at, id, c.Viewer.Session, c.Seq)
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
		res, err := tx.Exec(`INSERT INTO cursor_party_cursors (id, sid, name, hue, x, y, seq, born, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO NOTHING`, id, c.Viewer.Session, name, hue, c.X, c.Y, c.Seq, at, at)
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

A frame changes `--x` and `--y` of the cursors that moved, in percent of the floor, which is a size container: `calc(var(--x) * 1cqi)` is the place in pixels. Each part of a cursor eases there on its own. The name tag, which carries the arrow, takes 120 ms; the two sparks of the trail take 180 and 260 ms, so in motion they fall behind along the path, and at rest they hide under the arrow.

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

The tabs here are the streams that watch the party, as the hub counts them; the counter's view has `Presence`, so it renders again when a tab comes or goes. The moves are those of the last whole second. Both numbers are integers registered with `@property`, which CSS draws as counters, so when a frame changes one it rolls to the new value.

```css title=cursor-party.css
@property --cursor-party-rate {
	syntax: "<integer>";
	inherits: true;
	initial-value: 0;
}
```
