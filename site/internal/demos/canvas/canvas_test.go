package canvas

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demo/demotest"
)

var (
	ada = demo.Viewer{Session: "s1", Tab: "tab-of-ada-0123456789"}
	bob = demo.Viewer{Session: "s2", Tab: "tab-of-bob-0123456789"}
	t0  = time.Unix(1_800_000_000, 0)
)

// TestPaint paints a few cells and reads them back as the cells attribute,
// the pixels counted, and the painter's colour; a paint that changes no
// colour renews the cells' day but leaves the board's version alone.
func TestPaint(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, Paint{Viewer: ada, Color: 7, Cells: []int{0, 1, 2, Cells - 1}, At: t0})
	if got := e.Touched(); !slices.Equal(got, []string{TopicCells, TopicPainters, TopicPaints}) {
		t.Errorf("a first paint touched %v", got)
	}
	b, version := read(t, e)
	cells := hexOf(b)
	if !strings.HasPrefix(cells, "7770") || !strings.HasSuffix(cells, "07") || strings.Count(cells, "0") != Cells-4 {
		t.Fatalf("cells %s…%s", cells[:8], cells[len(cells)-8:])
	}
	if m := paintedAt(t, e, 1); m != minute(t0) || paintedAt(t, e, 3) != 0 {
		t.Errorf("painted at %d, %d", m, paintedAt(t, e, 3))
	}

	// The same colour again, a minute later: the day starts again, the
	// board does not change.
	e.Do(t, Paint{Viewer: ada, Color: 7, Cells: []int{1}, At: t0.Add(time.Minute)})
	if got := e.Touched(); !slices.Equal(got, []string{TopicPaints}) {
		t.Errorf("a repaint touched %v", got)
	}
	_, version2 := read(t, e)
	if m := paintedAt(t, e, 1); version2 != version || m != minute(t0)+1 {
		t.Errorf("a repaint: version %d to %d, painted at %d", version, version2, m)
	}

	// The background erases: the cell is blank and has no day.
	e.Do(t, Paint{Viewer: bob, Color: 0, Cells: []int{2}, At: t0.Add(time.Minute)})
	b3, _ := read(t, e)
	if b3.color(2) != 0 || paintedAt(t, e, 2) != 0 {
		t.Errorf("an erased cell: colour %d, painted at %d", b3.color(2), paintedAt(t, e, 2))
	}

	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		p, err := pulse(ctx, tx, t0.Add(time.Minute))
		if err != nil {
			return err
		}
		if p.Total != 6 || p.On != 3 || p.Minute != 2 || p.Seconds[Window-1] != 2 {
			t.Errorf("the pulse a minute later: %+v", p)
		}
		list, err := painters(ctx, tx, []string{ada.Tab, bob.Tab, "t3"}, t0.Add(time.Minute))
		if err != nil {
			return err
		}
		want := []Painter{{Tag: Tag(ada.Tab), Color: 7, Pixels: 1, Active: true}, {Tag: Tag(bob.Tab), Color: 0, Pixels: 1, Active: true}, {Tag: Tag("t3"), Color: -1}}
		for i := range want {
			list[i].id = 0
		}
		if !slices.Equal(list, want) {
			t.Errorf("painters %+v", list)
		}
		// Ten seconds on, nobody is painting any more.
		if list, err = painters(ctx, tx, []string{ada.Tab}, t0.Add(70*time.Second)); err != nil || list[0].Active || list[0].Pixels != 1 {
			t.Errorf("ten seconds later: %+v, %v", list, err)
		}
		return nil
	})
}

// A batch over the pixel rate paints nothing and records why in the tab's
// row, which the next batch that lands clears.
func TestPaintOver(t *testing.T) {
	e := demotest.Open(t, Demo)
	state := func() (s TabState, cells string) {
		t.Helper()
		e.View(t, func(ctx context.Context, tx *sql.Tx) error {
			if err := demo.LoadTab(ctx, tx, Slug, ada, &s); err != nil {
				return err
			}
			c, _, err := cellsOf(ctx, tx)
			cells = c
			return err
		})
		return s, cells
	}
	e.Do(t, Paint{Viewer: ada, Color: 3, Cells: []int{0}, Over: true, At: t0})
	if s, cells := state(); s.Refusal != TooFast || cells[0] != '0' {
		t.Fatalf("over the rate: tab %+v, cell 0 %q", s, cells[0])
	}
	if got := e.Touched(); !slices.Equal(got, []string{demo.TabTopic(Slug, ada.Tab)}) {
		t.Errorf("a refused batch touched %v", got)
	}
	e.Do(t, Paint{Viewer: ada, Color: 3, Cells: []int{0}, At: t0})
	if s, cells := state(); s.Refusal != "" || cells[0] != '3' {
		t.Fatalf("a batch that lands: tab %+v, cell 0 %q", s, cells[0])
	}
}

func TestPaintValidate(t *testing.T) {
	e := demotest.Open(t, Demo)
	many := make([]int, MaxPaint+1)
	for i := range many {
		many[i] = i
	}
	for name, p := range map[string]Paint{
		"no tab":       {Color: 1, Cells: []int{0}},
		"colour 16":    {Viewer: ada, Color: 16, Cells: []int{0}},
		"colour -1":    {Viewer: ada, Color: -1, Cells: []int{0}},
		"no cells":     {Viewer: ada, Color: 1},
		"61 cells":     {Viewer: ada, Color: 1, Cells: many},
		"off the side": {Viewer: ada, Color: 1, Cells: []int{Cells}},
		"negative":     {Viewer: ada, Color: 1, Cells: []int{-1}},
	} {
		if err := e.W.Do(context.Background(), p); !errors.Is(err, writer.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := e.W.Do(context.Background(), Pick{Viewer: ada, Color: 16}); !errors.Is(err, writer.ErrInvalid) {
		t.Errorf("pick colour 16: %v", err)
	}
}

// TestPick: a tab's first pick adds it to the painters, a new colour
// changes it, the same colour again changes nothing anyone sees.
func TestPick(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, Pick{Viewer: ada, Color: 7, At: t0})
	if got := e.Touched(); !slices.Equal(got, []string{TopicPainters}) {
		t.Errorf("a first pick touched %v", got)
	}
	e.Do(t, Pick{Viewer: ada, Color: 7, At: t0})
	if got := e.Touched(); len(got) != 0 {
		t.Errorf("the same colour again touched %v", got)
	}
	e.Do(t, Pick{Viewer: ada, Color: 14, At: t0.Add(time.Second)})
	if got := e.Touched(); !slices.Equal(got, []string{TopicPainters}) {
		t.Errorf("a new colour touched %v", got)
	}
	e.Do(t, Pick{Viewer: demo.Viewer{Session: ada.Session, Tab: "t9"}, Color: 3, At: t0.Add(2 * time.Second)})
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		// A session's next tab starts with the brush it used last.
		if c, err := lastColor(ctx, tx, ada.Session); err != nil || c != 3 {
			t.Errorf("the session's last colour: %d, %v", c, err)
		}
		if c, err := lastColor(ctx, tx, "new"); err != nil || c != DefaultColor {
			t.Errorf("a new session's colour: %d, %v", c, err)
		}
		return nil
	})
}

// TestSweep fades the pixels whose day is over, and only those, and
// forgets the last minute's old seconds and the painters gone a day.
func TestSweep(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, Paint{Viewer: ada, Color: 7, Cells: []int{10, 11}, At: t0})
	e.Do(t, Paint{Viewer: bob, Color: 9, Cells: []int{12}, At: t0.Add(2 * time.Hour)})
	e.Touched()

	// An hour before the first pixels' day ends: nothing fades yet, and
	// the pulse counts them as fading within the hour.
	e.Do(t, writer.Func(func(tx *writer.Tx) error { return sweep(tx, t0.Add(23*time.Hour)) }))
	if got := e.Touched(); !slices.Equal(got, []string{TopicPainters, TopicPaints}) {
		t.Errorf("a sweep of old seconds touched %v", got)
	}
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		p, err := pulse(ctx, tx, t0.Add(23*time.Hour))
		if p.On != 3 || p.Fading != 2 || p.Minute != 0 {
			t.Errorf("an hour before: %+v", p)
		}
		return err
	})

	e.Do(t, writer.Func(func(tx *writer.Tx) error { return sweep(tx, t0.Add(Lifetime+time.Minute)) }))
	if got := e.Touched(); !slices.Equal(got, []string{TopicCells, TopicPainters, TopicPaints}) {
		t.Errorf("the sweep touched %v", got)
	}
	b, _ := read(t, e)
	if b.color(10) != 0 || b.color(11) != 0 || b.color(12) != 9 || paintedAt(t, e, 10) != 0 || paintedAt(t, e, 12) == 0 {
		t.Errorf("after a day: %s", hexOf(b)[:8])
	}
	var painters int
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM canvas_painters`).Scan(&painters)
	})
	if painters != 1 {
		t.Errorf("%d painters after a day, want bob's", painters)
	}

	e.Do(t, writer.Func(func(tx *writer.Tx) error { return sweep(tx, t0.Add(Lifetime+time.Minute)) }))
	if got := e.Touched(); len(got) != 0 {
		t.Errorf("a sweep with nothing to do touched %v", got)
	}

	// A flood of tabs stops at the cap, the newest kept.
	e.Do(t, writer.Func(func(tx *writer.Tx) error {
		for i := range MaxPainters + 10 {
			if err := (Pick{Viewer: demo.Viewer{Session: "flood", Tab: fmt.Sprint("flood-", i)}, Color: 1, At: t0.Add(Lifetime)}).Apply(tx); err != nil {
				return err
			}
		}
		return nil
	}))
	e.Do(t, writer.Func(func(tx *writer.Tx) error { return sweep(tx, t0.Add(Lifetime+time.Minute)) }))
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM canvas_painters`).Scan(&painters)
	})
	if painters != MaxPainters {
		t.Errorf("%d painters after a flood, want %d", painters, MaxPainters)
	}
}

// The last minute stays a minute: a paint deletes the seconds that left
// it.
func TestRecentStaysSmall(t *testing.T) {
	e := demotest.Open(t, Demo)
	for s := range 200 {
		e.Do(t, Paint{Viewer: ada, Color: 1, Cells: []int{s}, At: t0.Add(time.Duration(s) * time.Second)})
	}
	var n int
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM canvas_recent`).Scan(&n)
	})
	if n != Window {
		t.Errorf("%d seconds kept, want %d", n, Window)
	}
}

// TestViews renders the three shared views and the document: the board
// with its cells and the wiring of a paint, the painters with their
// faces and colours, the pulse; the document adds the tab, its brush and
// its own mark.
func TestViews(t *testing.T) {
	e := demotest.Open(t, Demo)
	now := time.Now()
	e.Do(t, Paint{Viewer: ada, Color: 14, Cells: []int{0, 1}, At: now})
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		vs := pageViews(ada)
		if len(vs) != 4 || vs[0].Key != "canvas:board" || vs[1].Key != "canvas:here" || vs[2].Key != "canvas:pulse" || !vs[1].Presence || vs[3].Key != demo.TabTopic(Slug, ada.Tab) {
			t.Fatalf("views %+v", vs)
		}
		frames := make([]string, 3)
		for i, v := range vs[:3] {
			out, err := v.Render(ctx, tx)
			if err != nil {
				return err
			}
			frames[i] = string(out.([]byte))
		}
		for _, want := range []string{`id="canvas-board"`, `cells="ee00`, `size="64"`, `requestCancellation`, `data-preserve-attr="palette color"`} {
			if !strings.Contains(frames[0], want) {
				t.Errorf("the board has no %s:\n%.400s", want, frames[0])
			}
		}
		// No hub here: the view lists nobody, and still renders.
		if !strings.Contains(frames[1], "0 painters here") {
			t.Errorf("the painters:\n%s", frames[1])
		}
		if !strings.Contains(frames[2], `--canvas-n: 2`) || !strings.Contains(frames[2], "Painted since the start") {
			t.Errorf("the pulse:\n%s", frames[2])
		}
		page, err := renderPage(ctx, tx, ada)
		if err != nil {
			return err
		}
		var html strings.Builder
		if err := page.Render(ctx, &html); err != nil {
			return err
		}
		doc := html.String()
		for _, want := range []string{
			`{_canvas: {color: 14, palette: false`,          // the brush ada used last
			`data-tag="` + Tag(ada.Tab) + `" data-c="14"`,   // ada among the painters
			`.cv__painter[data-tag="` + Tag(ada.Tab) + `"]`, // and marked as the tab's own
			`value="14" checked`,
		} {
			if !strings.Contains(doc, want) {
				t.Errorf("the document has no %s", want)
			}
		}
		for _, f := range frames {
			if strings.Contains(f, ada.Tab) {
				t.Error("a shared view carries a tab id")
			}
		}

		// A new tab of the same session: among the painters already, in
		// the brush the session used last, before its first pick.
		fresh := demo.Viewer{Session: ada.Session, Tab: "tab-fresh-0123456789"}
		page, err = renderPage(ctx, tx, fresh)
		if err != nil {
			return err
		}
		html.Reset()
		if err := page.Render(ctx, &html); err != nil {
			return err
		}
		if !strings.Contains(html.String(), `data-tag="`+Tag(fresh.Tab)+`" data-c="14"`) {
			t.Error("a new tab is not among the painters in its brush")
		}
		return nil
	})
}

// The board is encoded once per version.
func TestCellsCache(t *testing.T) {
	e := demotest.Open(t, Demo)
	var first, again string
	e.View(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		first, _, err = cellsOf(ctx, tx)
		return err
	})
	e.View(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		again, _, err = cellsOf(ctx, tx)
		return err
	})
	if len(first) != Cells || unsafeData(first) != unsafeData(again) {
		t.Error("the same version was encoded twice")
	}
	e.Do(t, Paint{Viewer: ada, Color: 1, Cells: []int{5}, At: t0})
	e.View(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		again, _, err = cellsOf(ctx, tx)
		return err
	})
	if again[5] != '1' {
		t.Errorf("a new version kept the old encoding: %s", again[:8])
	}
}

func TestFace(t *testing.T) {
	seen := map[string]bool{}
	for i := range 200 {
		tag := Tag(strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('A'+i/26)))
		f := face(tag)
		if f == "" || f != face(tag) || !strings.HasPrefix(f, "M") {
			t.Fatalf("face of %s: %q", tag, f)
		}
		seen[f] = true
	}
	if len(seen) < 150 {
		t.Errorf("200 tabs share %d faces", len(seen))
	}
	if len(Tag("a")) != 10 || Tag("a") == Tag("b") {
		t.Error("tags")
	}
}

func TestThousands(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -4096: "-4,096"} {
		if got := thousands(n); got != want {
			t.Errorf("%d: %s", n, got)
		}
	}
}

func read(t *testing.T, e *demotest.Env) (b board, version int64) {
	t.Helper()
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT version, cells FROM canvas_board WHERE id = 1`).Scan(&version, (*[]byte)(&b))
	})
	return b, version
}

// paintedAt is the minute the cell was last painted, 0 for a blank one.
func paintedAt(t *testing.T, e *demotest.Env, cell int) (m int64) {
	t.Helper()
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT minute FROM canvas_painted WHERE cell = ?`, cell).Scan(&m)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return m
}

func hexOf(b board) string {
	var s strings.Builder
	for i := range Cells {
		s.WriteByte("0123456789abcdef"[b.color(i)])
	}
	return s.String()
}

// unsafeData tells two strings apart by their bytes' address: equal only
// when one is the other.
func unsafeData(s string) *byte { return unsafe.StringData(s) }
