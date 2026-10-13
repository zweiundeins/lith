## The clock

`Mount` starts a clock for the site, with the site's writer and hub. It looks for viewers four times a second, and while there are some it asks for a generation ten times a second. Each `Step` waits for its commit, so a busy writer slows the clock down instead of piling steps up in the queue. The clock ends when the hub stops (`Hub.Done`).

```go title=clock.go
func (c *clock) run() {
	t := time.NewTicker(Idle)
	defer t.Stop()
	running := false
	var logged time.Time // a broken database fails every step: log once a minute
	for {
		select {
		case <-c.hub.Done():
			return
		case <-t.C:
		}
		if watched := c.hub.Count(BoardKey) > 0; watched != running {
			running = watched
			if running {
				t.Reset(time.Second / Speed)
			} else {
				t.Reset(Idle)
			}
		}
		if !running {
			continue
		}
		err := c.w.Do(context.Background(), Step{At: time.Now()})
		switch {
		case errors.Is(err, writer.ErrClosed):
			return
		case err != nil && time.Since(logged) > time.Minute:
			c.log.Warn("life: a step failed", "err", err)
			logged = time.Now()
		}
	}
}
```

## The step

`Step` is a command like any other: it reads the board in the writer's transaction and writes it back, 0.14 ms with the commit on an i5-13500, of which the generation itself takes 0.03 ms. A quiet board, empty for 15 seconds or repeating itself for 30, gets a new soup. An empty board is left as it is, so the step writes nothing and no frame goes out.

```go title=commands.go
func (c Step) Apply(tx *writer.Tx) error {
	b, err := load(tx.Context(), tx)
	if err != nil {
		return err
	}
	now := c.At.UnixMilli()
	dirty := false
	switch {
	case b.Quiet != 0 && now-b.Quiet >= b.quietFor().Milliseconds():
		b.reseed(now)
		dirty = true
	case Population(b.Cells) == 0:
		// An empty board stays as it is: no generation, no frame.
		if b.Quiet == 0 {
			b.Quiet, dirty = now, true
		}
		if b.Rate != 0 {
			b.Rate, dirty = 0, true
		}
	default:
		b.step(now)
		dirty = true
	}
	if !dirty {
		return nil
	}
	return b.save(tx)
}
```

A live cell with two or three live neighbours lives on in its colour, and a dead cell with exactly three is born. The newborn takes the colour that two or three of its parents share. When all three differ, the cell's place and the generation pick one of them, so the same board always has the same next generation:

```go title=board.go
func majority(p [3]byte, tie int64) byte {
	switch {
	case p[0] == p[1] || p[0] == p[2]:
		return p[0]
	case p[1] == p[2]:
		return p[1]
	}
	return p[tie]
}
```

## The board's view

One view serves every tab. It renders after each step, when someone opens or leaves the page (`Presence`, for the count above the board), and every second while the board is quiet, for the countdown under it. Its frames morph without a view transition: at ten frames a second, each transition would cut the one before short.

```go title=live.go
func boardView() live.View {
	return live.View{
		Key:      BoardKey,
		Topics:   []string{Topic},
		Every:    time.Second,
		Presence: true,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			b, err := load(ctx, demo.Querier(ctx, tx))
			if err != nil {
				return nil, err
			}
			return demo.Patch(ctx, Frame(b, demo.Watching(ctx, BoardKey), time.Now()))
		},
	}
}
```

## Cells of one length

Every frame holds the whole board, and every cell in it is a `<p>` with a class of one letter. A `<p>` ends the one before it, so a cell needs no end tag, and all cells have the same length, 11 bytes. A frame then lines up byte for byte with the one before it, and zstd encodes little more than the cells that changed: 465 bytes a frame on a soup. With dead cells as a bare `<p>`, a frame is 15 KB instead of 45, but each birth and death shifts the rest of it, and the same frames take 784 bytes.

```go title=render.go
var cellTags = func() (t [len(cellClass)]string) {
	for i := range t {
		t[i] = "<p class=" + cellClass[i:i+1] + ">"
	}
	return t
}()
```

## Births and deaths in CSS

The morph changes the class of each cell born or died, and CSS animates the change. A cell is a square of its colour painted in the middle of its box, a circle in the smooth style, sized with `background-size`. A cell that dies shrinks and cools into its dot in three steps, one each generation, which leaves a short trail behind whatever moves; a cell born grows in two.

```css title=life.css
& > p {
	color: var(--life-dot);
	background: radial-gradient(circle closest-side, currentColor calc(92% + 60% * var(--lith-notch)), transparent calc(100% + 60% * var(--lith-notch))) 50% 50% / 24% 24% no-repeat;
	transition:
		color 300ms steps(3, start),
		background-size 300ms steps(3, start);
}

& > p:not(._) {
	color: var(--c);
	background-size: 86% 86%;
	transition-duration: 100ms;
	transition-timing-function: steps(2, start);
}
```

The page morphs 4,000 elements ten times a second, so what the browser does per cell counts. A transform such as `scale` gives an element a paint layer of its own; a gradient sized with `background-size` needs none. A stepped transition changes a cell's look only at its steps, so the browser repaints it two or three times instead of every frame. In headless Chromium on an i5-13500, a tab on a fresh soup keeps its main thread 30 to 40% busy without the transitions and 65 to 85% with them (`TestLifePageCost`).

## Drawing

While you drag, the page collects the cells under the pointer and sends them every 100 ms, joined by a line to the last cell it sent, so a fast stroke has no gaps. The cells sent show as a path in your colour until your tab's view confirms the op; the board's frame of the same tick holds the cells themselves.

```go title=expr.go
const flush = `$_life.stroke !== '' && (
	$_life.problem = '',
	` + newOp + `,
	$_life.pend += $_life.draft,
	$_life.draft = '',
	@post('/cmd/life/draw', {payload: {tab: $_tab, op: $_life.op, cells: ($_life.last >= 0 ? $_life.last + ',' + $_life.stroke : $_life.stroke).split(',').map(Number), alive: $_life.alive}, requestCancellation: 'disabled'}),
	$_life.last = Number($_life.stroke.split(',').pop()),
	$_life.stroke = '',
	$_life.sent = Date.now()
)`
```

On a narrow screen CSS stands the board on its side: the same 80 by 50 cells in 50 columns of 80, half again as large on a phone, and the pointer's maths swap the axes.

## Measured

On an i5-13500 on 13 October 2026, with the site running as in production (`GOMEMLIMIT=192MiB`, on four cores) and the streams opened by `TestLoad` in another process:

| Streams | CPU per generation | Bytes a frame per stream | Resident |
| --- | --- | --- | --- |
| 1 | 0.8 ms | 476 | 46 MB |
| 10 | 1.2 ms | 425 | 76 MB |
| 100 | 7.7 ms | 404 | 187 MB |

Every stream got ten frames a second and none was dropped. With 100 streams the server used 0.08 cores, and the streams' zstd encoders held 87 MB, 2^18 windows in low-memory mode as the site's budget hands them out. Go collects when the heap has doubled since the last collection, so the process holds up to twice what lives, and `GOMEMLIMIT` keeps that under 192 MiB.

## Credits

Anders Murphy built this first with Datastar: a shared Game of Life rendered on the server and streamed whole to every browser, in ["Realtime collaborative web apps without ClojureScript"](https://andersmurphy.com/2025/04/07/clojure-realtime-collaborative-web-apps-without-clojurescript.html). The same board runs on php-via at [gameoflife.zweiundeins.gmbh](https://gameoflife.zweiundeins.gmbh).
