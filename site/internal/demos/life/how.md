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

`Step` is a command like any other: it reads the board in the writer's transaction and writes it back, 0.14 ms with the commit on a development machine (an Intel i5-13500), of which the generation itself takes 0.03 ms. A quiet board, empty for 15 seconds or repeating itself for 30, gets a new soup. An empty board is left as it is, so the step writes nothing and no frame goes out.

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

Every frame holds the whole board, and every cell in it is a `<p>` with a class of one letter: its colour, or its trail's. A `<p>` ends the one before it, so a cell needs no end tag, and all cells have the same length, 11 bytes. A frame then lines up byte for byte with the one before it, and zstd encodes little more than the cells that changed: 465 bytes a frame on a soup. With dead cells as a bare `<p>`, a frame is 15 KB instead of 45, but each birth and death shifts the rest of it, and the same frames take 784 bytes.

```go title=render.go
var cellTags = func() (t [3][len(cellClass)]string) {
	for age, class := range [...]string{cellClass, dyingClass, trailClass} {
		for i := range class {
			t[age][i] = "<p class=" + class[i:i+1] + ">"
		}
	}
	return t
}()
```

## Rows with ids

The board is 50 rows of 80 cells, and each row has an id. Datastar's morph pairs an element that has an id with the element of the same id, so a frame's morph walks 50 known rows instead of matching 4,000 cells against each other. The ids also give zstd the same bytes at the same places in every frame, which makes a frame about 30% smaller.

```go title=render.go
var rowTags = func() (t [H]string) {
	for y := range t {
		t[y] = `<div id="life-r` + strconv.Itoa(y) + `">`
	}
	return t
}()
```

CSS lays the rows out as one grid: a row is `display: contents`, so its cells are the grid's items.

## The trail

A cell that dies shrinks and cools into its dot over two generations, which leaves a short trail behind whatever moves. The server draws it: each step keeps, beside the cells, the colour of every cell that died one or two generations ago, and the frame gives such a cell a class of its own, its colour's capital a generation after it died and another letter the generation after. Every class is still one letter, so the cells keep their length.

```go title=state.go
func trail(old, cur, next []byte) []byte {
	t := make([]byte, Cells)
	for i, c := range next {
		switch {
		case c != 0:
		case cur[i] != 0:
			t[i] = 1<<4 | cur[i]
		case len(old) == Cells && old[i]>>4 == 1:
			t[i] = 2<<4 | old[i]&15
		}
	}
	return t
}
```

## The board in CSS

A cell is a square of its colour painted in the middle of its box, a circle in the smooth style, sized with `background-size`. A transform such as `scale` would give each of the 4,000 cells a paint layer of its own; a gradient needs none. Nothing on the board animates: each class has its size and its colour, and the trail's colours are mixed into the dot's.

```css title=life.css
& > div > :is(.a, .b, .c, .d, .e, .f, .g, .h, .w) {
	color: var(--c);
	background-size: 86% 86%;
}

& > div > :is(.A, .B, .C, .D, .E, .F, .G, .H, .W) {
	color: color-mix(in oklab, var(--c) 67%, var(--life-dot));
	background-size: 65% 65%;
}
```

## What a frame costs the browser

Every frame is the whole board, and the page morphs it into the one before. A trail drawn with CSS transitions (300 ms in three steps) costs the browser more than the morph itself: with Chromium's CPU slowed down four times, as DevTools throttles it to stand in for a slow phone, a tab then applies only 32 to 62 of the 100 frames sent in 10 seconds. With the trail rendered on the server it applies every one. In headless Chromium on an i5-13500, over 10 seconds of a fresh soup (`TestLifePageCost`):

| | Frames applied (of 100) | Main thread | Per frame |
| --- | --- | --- | --- |
| Transitions, CPU as is | 100 | 65 to 85% busy | |
| Transitions, CPU 4x slower | 32 to 62 | 100% busy | 120 to 240 ms |
| Server trail, CPU as is | 100 | 36 to 40% busy | 14 to 16 ms script, 6 to 7 ms style |
| Server trail, CPU 4x slower | 100 | 99 to 100% busy | 45 to 47 ms script, 19 to 20 ms style |

The ids alone change little in the browser: without transitions, a frame costs the same with ids and without, and two or three ids a row cost the same as one, in a few more bytes. Datastar's `replace` mode for the board costs more than the morph: it builds 4,000 new elements each frame and lays them out again, 100 to 135 ms of script a frame at 4x. On the wire the trail spends about what the ids save, since it changes more cells each frame.

On a narrow screen CSS stands the board on its side: the same 80 by 50 cells in 50 columns of 80, half again as large on a phone, and the pointer's maths swap the axes.

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

## Measured

On a development machine, an Intel i5-13500, on 13 October 2026, with the site running as in production (`GOMEMLIMIT=192MiB`, on four cores) and the streams opened by `TestLoad` in another process, 20 to 30 seconds a run:

| Streams | CPU per generation | Bytes a frame per stream | Resident |
| --- | --- | --- | --- |
| 1 | 0.8 to 3 ms | 210 to 520 | 41 to 49 MB |
| 10 | 1.2 to 6 ms | 280 to 520 | 68 to 77 MB |
| 100 | 7.7 to 28 ms | 280 to 560 | 166 to 190 MB |

Every stream got ten frames a second and none was dropped. The bytes depend on the soup: a young one changes more cells each generation. The CPU time follows the clock of the cores, which ran at 0.9 to 2.2 GHz in the slower runs, idle between frames. Measured back to back, where the cores stay fast, a generation costs 0.3 ms for the step and the render, and compressing it costs 30 to 40 µs for each stream (`BenchmarkCompress`).

With 100 streams, the streams' zstd encoders held 87 MB, 2^18 windows in low-memory mode as the site's budget hands them out. Go collects when the heap has doubled since the last collection, so the process holds up to twice what lives, and `GOMEMLIMIT` keeps that under 192 MiB. With nobody watching, the server spent 10 to 20 ms of CPU in 10 seconds, on the site as a whole.

## Credits

Anders Murphy built this first with Datastar: a shared Game of Life rendered on the server and streamed whole to every browser, in ["Realtime collaborative web apps without ClojureScript"](https://andersmurphy.com/2025/04/07/clojure-realtime-collaborative-web-apps-without-clojurescript.html). The same board runs on php-via at [gameoflife.zweiundeins.gmbh](https://gameoflife.zweiundeins.gmbh).
