## The move command

Every move is a `Move` command, and the writer is the only code that writes. The page sends the version of the card it shows, from the card's `data-version`, and `Apply` checks it in the same transaction that moves the card:

```go title=commands.go
	if version != c.Seen {
		// Someone moved the card after this page drew it: the later move
		// loses, and only its tab hears why.
		state.Refusal = refusal(title, lane, by, c.Who)
		if state.Version, err = boardVersion(tx); err != nil {
			return err
		}
		return demo.SaveTab(tx, Slug, c.Viewer, state, c.At)
	}
```

Of two people who drop the same card at once, the writer applies one move first, and that move gives the card a new version: the second one is refused. A refusal is not an error. The command commits with the reason in the tab's row and the board unchanged, so only that tab's view renders, and it names who was faster. A move that goes through takes the card out of its lane, makes room in the other with two `UPDATE`s, and touches `board`.

## The board's view

The board is one view for everyone, keyed `board:board`. It renders once in each tick in which the `board` topic changed, and every tab that shows the board gets that frame.

```go title=live.go
func boardView() live.View {
	return live.View{
		Key:    boardKey,
		Topics: []string{Topic},
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			b, err := readBoard(ctx, demo.Querier(ctx, tx))
			if err != nil {
				return nil, err
			}
			f, err := demo.Animated(ctx, LanesView(b, time.Now()), sse.Patch{})
			if err != nil {
				return nil, err
			}
			v, err := demo.Signals(versionSignal(b.Version))
			if err != nil {
				return nil, err
			}
			f.First, f.Next = append(f.First, v...), append(f.Next, v...)
			track.rendered(time.Now(), b.Version, demo.Watching(ctx, boardKey))
			return f, nil
		},
	}
}
```

`demo.Animated` renders the frame in two forms: a tab's first frame morphs at once, every later one inside a view transition. The frame also sets the signal `$_board.v` to the board's version, and the render tells the numbers above the board how many streams watch it.

## The pending move

Dropping a card moves nothing on the page. The page sends the move, leaves the card where it was, faded, with a pixel walking in its corner, and marks the place it goes with a bar of light. The pending state ends once the tab's outcome has come with the same op id, and the board shows at least the version that outcome names:

```go title=expr.go
const settle = `const op = $_board.op
if (!op || $_board.ack !== op || $_board.v < $_board.ackv) return
const card = $_board.pcard
if (!$_board.ok) {
	$_board.nudge = card
	setTimeout(() => { if ($_board.nudge === card) $_board.nudge = 0 }, 900)
}
$_board.op = ''
$_board.pcard = 0
$_board.problem = ''
if (!$_board.drag) el.bd.aim(-1, 0)`
```

Both come in one write. The move touches the board's topic and the tab's, the hub renders both views in the same tick, and the site writes a tick's frames to a stream at once, the board first. The faded card and the bar are a style element outside the regions the stream morphs, built from these signals, so no frame takes them away early.

## The view transition

Each card has a `view-transition-name` of its own, `board-card-` and its id, and the class `board-card`. When a frame moves cards, each card's group glides from its old box to its new one, and the card that moved carries the class `board-moved` as well:

```css title=board.css
::view-transition-group(.board-card) {
	animation-duration: var(--lith-dur-5);
	animation-timing-function: var(--lith-ease-glide);
}

::view-transition-old(.board-card) {
	animation: none;
	opacity: 0;
}

::view-transition-new(.board-card) {
	animation: none;
}
```

The moved card's group gets `z-index: 1`, so it flies over the other lanes, and rises a little on the way. While a page drags a card of its own, other moves snap into place there instead: a transition holds the page still while it runs, the dragged copy included. During a transition Chromium hands every press to the root element, so the drag finds the card under the pointer by its box.

## Who is on the board

A tab says hello when its stream opens, and the hello stores the tab with a hash of its session in `board_here`. The hub knows which of those tabs are still open: every tab watches a view of its own, keyed by its tab, and the hub counts the streams that watch a view.

```go title=live.go
func streamOpen(ctx context.Context) func(tab string) bool {
	return func(tab string) bool { return demo.Watching(ctx, demo.TabTopic(Slug, tab)) > 0 }
}
```

The row of avatars is a view with `Presence: true`, which the hub renders again whenever a stream starts or stops watching it, so an avatar leaves with its last tab.

## The numbers above the board

Every render of the board records itself: how many moves it took in, from the change of the board's version, and how many streams were watching, one frame each.

```go title=stats.go
func (t *tracker) rendered(now time.Time, version int64, viewers int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	moves := 0
	if t.seen && version > t.version {
		moves = int(version - t.version)
	}
	t.seen, t.version = true, version
	if moves > 0 {
		t.last = Tick{Moves: moves, Frames: viewers, At: now}
	}
	s := t.at(now.Unix())
	s.moves += moves
	s.renders++
	s.frames += viewers
}
```

The counts live in memory, a minute of them, and start over with the process. The view of the numbers renders every second (`Every: time.Second`), and a second whose numbers did not change sends nothing: the hub drops a frame that equals the last.

## The reset after a quiet hour

The sweep runs every five minutes. Once the board has gone an hour without a move, it puts the cards back where they started:

```go title=commands.go
	if _, err := tx.Exec(`UPDATE board_cards SET lane = start_lane, pos = start_pos, version = version + 1, moved_by = '', moved_at = 0
		WHERE lane != start_lane OR pos != start_pos OR moved_by != ''`); err != nil {
```

Every card it moves gets a new version, so a page that still shows the old board cannot move that card, and hears why.

## Measured

On the site's binary, in a process of its own with the production unit's `GOMEMLIMIT` of 192 MiB, on an Intel i5-13500 in October 2026: 100 tabs on the board, each stream compressed with zstd as Chromium asks for it, and one of the tabs moving a card a second. Over four runs the server used 0.8 to 1.3% of one core, 8 to 13 ms of CPU for each move and its 100 frames. The board's frame is about 10.7 KB of HTML and reached each tab in about 140 bytes, since each stream's zstd window still holds the frame before; with the numbers' frame, each tab received about 180 bytes a second. With nobody moving, the 100 tabs cost 0.2 to 0.45% of a core. Their streams' encoders held 83 MiB, the process 170 to 180 MiB resident. `TestBoardLoad` in `internal/app` measures it again.
