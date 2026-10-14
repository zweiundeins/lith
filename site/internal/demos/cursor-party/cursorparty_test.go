package cursorparty

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demo/demotest"
)

var (
	ada = demo.Viewer{Session: "s1", Tab: "tab-ada"}
	t0  = time.UnixMilli(1_800_000_000_000)
)

func TestMove(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, Move{Viewer: ada, X: 0.25, Y: 0.5, Seq: 1, At: t0})
	id, name, hue := Identity(ada.Tab)
	list := read(t, e, t0)
	if len(list) != 1 || list[0] != (Cursor{ID: id, Name: name, Hue: hue, A: floorAnchor, X: 0.25, Y: 0.5, At: t0}) {
		t.Fatalf("after the first move: %+v", list)
	}
	if got := e.Touched(); !slices.Equal(got, []string{Topic}) {
		t.Errorf("a move touched %v", got)
	}

	e.Do(t, Move{Viewer: ada, X: 0.75, Y: 0.125, Seq: 3, At: t0.Add(time.Second)})
	if list := read(t, e, t0.Add(time.Second)); list[0].X != 0.75 || list[0].Y != 0.125 || !list[0].At.Equal(t0.Add(time.Second)) {
		t.Errorf("after the second move: %+v", list)
	}
	e.Touched()

	// A move that a newer one overtook changes nothing and touches nothing.
	e.Do(t, Move{Viewer: ada, X: 0.5, Y: 0.5, Seq: 2, At: t0.Add(2 * time.Second)})
	if list := read(t, e, t0.Add(time.Second)); list[0].X != 0.75 {
		t.Errorf("an older move won: %+v", list)
	}
	// Nor does a tab id that another session sends.
	e.Do(t, Move{Viewer: demo.Viewer{Session: "s2", Tab: ada.Tab}, X: 0.1, Y: 0.1, Seq: 9, At: t0.Add(2 * time.Second)})
	if list := read(t, e, t0.Add(time.Second)); list[0].X != 0.75 {
		t.Errorf("another session moved Ada's cursor: %+v", list)
	}
	if got := e.Touched(); len(got) != 0 {
		t.Errorf("moves that changed nothing touched %v", got)
	}

	for name, m := range map[string]Move{
		"off the floor": {Viewer: ada, X: 1.01, Y: 0.5, Seq: 4},
		"below it":      {Viewer: ada, X: 0.5, Y: -0.1, Seq: 4},
		"no number":     {Viewer: ada, X: 0.5, Y: 0.5},
		"a bad tab":     {Viewer: demo.Viewer{Session: "s1", Tab: "a tab!"}, X: 0.5, Y: 0.5, Seq: 4},
		"no session":    {Viewer: demo.Viewer{Tab: "t"}, X: 0.5, Y: 0.5, Seq: 4},
	} {
		if err := e.W.Do(context.Background(), m); !errors.Is(err, writer.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestHistory counts the moves of each second: the counter shows the
// whole seconds before now, the last of them as moves a second.
func TestHistory(t *testing.T) {
	e := demotest.Open(t, Demo)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	for i, ms := range []int{0, 300, 900, 1100, 3500} {
		e.Do(t, Move{Viewer: ada, X: 0.5, Y: 0.5, Seq: int64(i + 1), At: at(ms)})
	}
	read := func(now time.Time) (h []int) {
		e.View(t, func(ctx context.Context, tx *sql.Tx) (err error) {
			h, err = history(ctx, tx, now)
			return err
		})
		return h
	}
	h := read(at(4000))
	if len(h) != History || !slices.Equal(h[History-4:], []int{3, 1, 0, 1}) || rate(h) != 1 {
		t.Errorf("four seconds in: %v", h)
	}
	if h := read(at(3999)); rate(h) != 0 || h[History-3] != 3 {
		t.Errorf("the second under way counts: %v", h)
	}
	if h := read(at(60_000)); slices.Max(h) != 0 {
		t.Errorf("a minute later: %v", h)
	}
	if got := sparkTop(h); got != "15" {
		t.Errorf("the sparkline's top of a quiet party: %s", got)
	}
}

// TestLayer renders the view as streams get it: at most Shown cursors,
// the newest, in the order of their ids; quiet ones marked idle, gone ones
// left out.
func TestLayer(t *testing.T) {
	e := demotest.Open(t, Demo)
	now := time.Now()
	move := func(tx *writer.Tx, tab string, x float64, at time.Time) error {
		return Move{Viewer: demo.Viewer{Session: "s", Tab: tab}, X: x, Y: 0.5, Seq: 1, At: at}.Apply(tx)
	}
	e.Do(t, writer.Func(func(tx *writer.Tx) error {
		// Five cursors that arrived first, one gone, one that has been
		// quiet for a while, and Shown-1 that arrived after them; then the
		// first five move again.
		for i := range 5 {
			if err := move(tx, fmt.Sprintf("old-%d", i), 0.5, now.Add(-5*time.Second)); err != nil {
				return err
			}
		}
		if err := move(tx, "gone", 0.5, now.Add(-Gone-time.Second)); err != nil {
			return err
		}
		if err := move(tx, "quiet", 0.05, now.Add(-Idle-500*time.Millisecond)); err != nil {
			return err
		}
		for i := range Shown - 1 {
			if err := move(tx, fmt.Sprintf("tab-%d", i), float64(i)/100, now.Add(-time.Second+time.Duration(i)*time.Millisecond)); err != nil {
				return err
			}
		}
		for i := range 5 {
			v := demo.Viewer{Session: "s", Tab: fmt.Sprintf("old-%d", i)}
			if err := (Move{Viewer: v, X: 0.5, Y: 0.5, Seq: 2, At: now}).Apply(tx); err != nil {
				return err
			}
		}
		return nil
	}))
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		vs := views(ada)
		if len(vs) != 2 || vs[0].Key != "cursor-party:cursors" || vs[1].Key != "cursor-party:count" || !vs[1].Presence {
			t.Fatalf("views: %+v", vs)
		}
		out, err := vs[0].Render(ctx, tx)
		if err != nil {
			return err
		}
		frame := string(out.([]byte))
		ids := regexp.MustCompile(`<i id="cursor-party-([0-9a-f]{12})"`).FindAllStringSubmatch(frame, -1)
		if len(ids) != Shown {
			t.Fatalf("%d cursors in the layer, want %d", len(ids), Shown)
		}
		if !slices.IsSortedFunc(ids, func(a, b []string) int { return strings.Compare(a[1], b[1]) }) {
			t.Error("the cursors are not in the order of their ids")
		}
		for i := range 5 {
			if id, _, _ := Identity(fmt.Sprintf("old-%d", i)); strings.Contains(frame, id) {
				t.Errorf("old-%d arrived before the newest %d and is in the layer", i, Shown)
			}
		}
		if id, _, _ := Identity("gone"); strings.Contains(frame, id) {
			t.Error("a cursor gone quiet is in the layer")
		}
		quiet, _, _ := Identity("quiet")
		if !regexp.MustCompile(`id="cursor-party-` + quiet + `" data-h="\d" style="--x:5.0;--y:50.0" data-idle><b>`).MatchString(frame) {
			t.Errorf("the quiet cursor is not marked idle:\n%s", frame)
		}
		if n := strings.Count(frame, "data-idle"); n != 1 {
			t.Errorf("%d cursors marked idle", n)
		}
		if !strings.HasPrefix(frame, "event: datastar-patch-elements\n") || strings.Contains(frame, "useViewTransition") {
			t.Errorf("the layer's frame:\n%.200s", frame)
		}
		return nil
	})
}

func TestCount(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, Move{Viewer: ada, X: 0.5, Y: 0.5, Seq: 1, At: time.Now().Add(-time.Second)})
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		out, err := countView().Render(ctx, tx)
		if err != nil {
			return err
		}
		frame := string(out.([]byte))
		// No hub in this test: nobody is counted here. The move a second
		// ago is the last point, or the one before when a second began
		// meanwhile.
		for _, want := range []string{`id="cursor-party-count"`, `--cursor-party-here:0;--cursor-party-rate:`, `people here, `, ` moves a second`,
			`<sb-sparkline values="[0,0,`, `" length="30" scale="fixed" min="0" max="15" aria-hidden="true">`} {
			if !strings.Contains(frame, want) {
				t.Errorf("no %q in the counter:\n%s", want, frame)
			}
		}
		if !strings.Contains(frame, `,0,1]"`) && !strings.Contains(frame, `,1,0]"`) {
			t.Errorf("the move is not in the sparkline:\n%s", frame)
		}
		return nil
	})
	if got := countText(1, 15); got != "1 person here, 15 moves a second" {
		t.Errorf("the counter's text: %q", got)
	}
}

// TestPage renders the demo's page and the landing page's part: the
// floor with its listeners, the layer, the counter, and the style that
// hides the viewer's own cursor.
func TestPage(t *testing.T) {
	e := demotest.Open(t, Demo)
	bob := demo.Viewer{Session: "s2", Tab: "tab-bob"}
	e.Do(t, Move{Viewer: ada, X: 0.25, Y: 0.5, Seq: 1, At: time.Now()})
	e.Do(t, Move{Viewer: bob, X: 0.75, Y: 0.5, Seq: 1, At: time.Now()})
	adaID, _, _ := Identity(ada.Tab)
	bobID, _, _ := Identity(bob.Tab)
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		for name, render := range map[string]func(context.Context, *sql.Tx, demo.Viewer) (templ.Component, error){"page": renderPage, "embed": renderEmbed} {
			c, err := render(ctx, tx, ada)
			if err != nil {
				return err
			}
			var b strings.Builder
			if err := c.Render(ctx, &b); err != nil {
				return err
			}
			html := b.String()
			for _, want := range []string{
				`<style>#cursor-party-` + adaID + `{display:none}</style>`,
				`id="cursor-party-` + adaID + `"`, // in the layer, hidden by the style
				`id="cursor-party-` + bobID + `"`,
				`data-on:pointermove__window__throttle.67ms=`,
				`data-on:pointermove__window__debounce.200ms=`,
				`data-signals="{_cursorParty: {seq: 0}}"`,
				`id="cursor-party-count"`,
				`--cursor-party-here:1;`,
				`<sb-sparkline values="[`,
			} {
				if !strings.Contains(html, want) {
					t.Errorf("%s: no %s in\n%s", name, want, html)
				}
			}
			if strings.Contains(html, bobID+`{display:none}`) {
				t.Errorf("%s: Bob's cursor is hidden from Ada", name)
			}
		}
		return nil
	})
}

// TestRoom keeps the table small between sweeps: a new cursor makes room,
// deleting the rows of tabs quiet for Keep and the oldest beyond
// MaxCursors.
func TestRoom(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, writer.Func(func(tx *writer.Tx) error {
		for i := range MaxCursors + 10 {
			v := demo.Viewer{Session: "s", Tab: fmt.Sprintf("tab-%d", i)}
			at := t0.Add(time.Duration(i) * time.Millisecond)
			if i < 3 {
				at = t0.Add(-Keep - time.Minute)
			}
			if _, err := tx.Exec(`INSERT INTO cursor_party_cursors (id, sid, name, hue, x, y, seq, born, at) VALUES (?1, ?2, 'ash owl', 5, 0.5, 0.5, 1, ?3, ?3)`,
				must(Identity(v.Tab)), v.Session, at.UnixMilli()); err != nil {
				return err
			}
		}
		return nil
	}))
	e.Do(t, Move{Viewer: ada, X: 0.5, Y: 0.5, Seq: 1, At: t0.Add(time.Minute)})
	n := rows(t, e)
	if n != MaxCursors {
		t.Errorf("%d rows after a new cursor, want %d", n, MaxCursors)
	}
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		var oldest int64
		if err := tx.QueryRowContext(ctx, `SELECT min(at) FROM cursor_party_cursors`).Scan(&oldest); err != nil {
			return err
		}
		// The three quiet ones went, and the eight oldest of the rest.
		if want := t0.Add(11 * time.Millisecond).UnixMilli(); oldest != want {
			t.Errorf("the oldest row moved at %d, want %d", oldest, want)
		}
		return nil
	})
}

func TestSweep(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, Move{Viewer: ada, X: 0.5, Y: 0.5, Seq: 1, At: t0})
	e.Do(t, Move{Viewer: demo.Viewer{Session: "s", Tab: "later"}, X: 0.5, Y: 0.5, Seq: 1, At: t0.Add(time.Minute)})
	e.Touched()
	e.Do(t, writer.Func(func(tx *writer.Tx) error { return sweep(tx, t0.Add(Keep+time.Second)) }))
	if n := rows(t, e); n != 1 {
		t.Errorf("%d rows after the sweep", n)
	}
	var seconds int
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM cursor_party_seconds`).Scan(&seconds)
	})
	if seconds != 1 {
		t.Errorf("%d seconds after the sweep, want the one a minute later", seconds)
	}
	if got := e.Touched(); !slices.Equal(got, []string{Topic}) {
		t.Errorf("the sweep touched %v", got)
	}
	e.Do(t, writer.Func(func(tx *writer.Tx) error { return sweep(tx, t0.Add(Keep+time.Second)) }))
	if got := e.Touched(); len(got) != 0 {
		t.Errorf("a sweep that deleted nothing touched %v", got)
	}
}

func TestIdentity(t *testing.T) {
	id, name, hue := Identity("tab-ada")
	id2, name2, hue2 := Identity("tab-ada")
	if id != id2 || name != name2 || hue != hue2 {
		t.Error("a tab's identity changes")
	}
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(id) || hue < 0 || hue >= len(hues) || !strings.HasPrefix(name, hues[hue]+" ") {
		t.Errorf("identity %q %q %d", id, name, hue)
	}
	if strings.Contains(id, "tab-ada") || id == must(Identity("tab-bob")) {
		t.Error("the id gives the tab away, or two tabs share it")
	}
	if css := selfStyle("tab-ada"); css != "#cursor-party-"+id+"{display:none}" {
		t.Errorf("the style that hides the tab's own cursor: %q", css)
	}
}

func TestPercent(t *testing.T) {
	for f, want := range map[float64]string{0: "0.0", 1: "100.0", 0.25: "25.0", 0.12345: "12.3", 0.99999: "100.0", 0.4567: "45.7"} {
		if got := string(percent(nil, f)); got != want {
			t.Errorf("percent(%g) = %s, want %s", f, got, want)
		}
	}
}

func read(t *testing.T, e *demotest.Env, now time.Time) (list []Cursor) {
	t.Helper()
	e.View(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		list, err = cursors(ctx, tx, now)
		return err
	})
	return list
}

func rows(t *testing.T, e *demotest.Env) (n int) {
	t.Helper()
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM cursor_party_cursors`).Scan(&n)
	})
	return n
}

func must(id, _ string, _ int) string { return id }

// A move beside an anchor keeps it, and the layer names it: every page
// puts the cursor by that anchor's box on its own layout.
func TestAnchoredMove(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, Move{Viewer: ada, A: "H", X: 1.5, Y: -0.25, Seq: 1, At: t0})
	list := read(t, e, t0)
	if len(list) != 1 || list[0].A != "H" || list[0].X != 1.5 || list[0].Y != -0.25 {
		t.Fatalf("after the move: %+v", list)
	}
	if got := string(layer(list, t0)); !strings.Contains(got, `data-a="H" style="--x:150.0;--y:-25.0"`) {
		t.Errorf("the layer: %s", got)
	}
	for _, m := range []Move{
		{Viewer: ada, A: "nope", X: 0.5, Y: 0.5, Seq: 2, At: t0},
		{Viewer: ada, A: "art", X: 21, Y: 0.5, Seq: 2, At: t0},
	} {
		if m.Validate() == nil {
			t.Errorf("%+v passed", m)
		}
	}
}
