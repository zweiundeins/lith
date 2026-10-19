package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demo/demotest"
)

var (
	ada = demo.Viewer{Session: "session-ada", Tab: "tab-ada"}
	bob = demo.Viewer{Session: "session-bob", Tab: "tab-bob"}
	at  = time.Unix(1_800_000_000, 0)
)

// layout is the board as card ids per lane, top to bottom.
func layout(t *testing.T, e *demotest.Env) [Lanes][]int64 {
	t.Helper()
	var l [Lanes][]int64
	b := read(t, e)
	for i, lane := range b.Lanes {
		for j, c := range lane {
			if c.Pos != j {
				t.Fatalf("lane %d: card %d at pos %d, want %d", i, c.ID, c.Pos, j)
			}
			l[i] = append(l[i], c.ID)
		}
	}
	return l
}

func read(t *testing.T, e *demotest.Env) (b Board) {
	t.Helper()
	e.View(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		b, err = readBoard(ctx, tx)
		return err
	})
	return b
}

func tab(t *testing.T, e *demotest.Env, v demo.Viewer) (s TabState) {
	t.Helper()
	e.View(t, func(ctx context.Context, tx *sql.Tx) error { return demo.LoadTab(ctx, tx, Slug, v, &s) })
	return s
}

func move(v demo.Viewer, op string, card int64, lane, index int, seen int64) Move {
	return Move{Viewer: v, Who: WhoOf(v.Session), Op: op, Card: card, Lane: lane, Index: index, Seen: seen, At: at}
}

func TestStart(t *testing.T) {
	e := demotest.Open(t, Demo)
	want := [Lanes][]int64{{1, 2, 3, 4, 5}, {6, 7, 8}, {9, 10, 11, 12}}
	if got := layout(t, e); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the starting layout: %v", got)
	}
	if b := read(t, e); b.Version != 0 || b.Last.ID != 0 || b.LastBy != "" {
		t.Errorf("a new board: %+v", b)
	}
}

func TestMove(t *testing.T) {
	e := demotest.Open(t, Demo)
	// To another lane: card 4 (to do, third from the top's next) into
	// doing, between 7 and 8.
	e.Do(t, move(ada, "op1", 4, 1, 2, 0))
	if got, want := layout(t, e), [Lanes][]int64{{1, 2, 3, 5}, {6, 7, 4, 8}, {9, 10, 11, 12}}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after a move across lanes: %v", got)
	}
	if got := e.Touched(); !slices.Equal(got, []string{"board", "tab:board:tab-ada"}) {
		t.Errorf("a move touched %v", got)
	}
	b := read(t, e)
	if b.Version != 1 || b.Last.ID != 4 || b.LastBy != WhoOf(ada.Session) || b.Lanes[1][2].Version != 1 || b.Lanes[1][2].MovedBy != WhoOf(ada.Session) {
		t.Errorf("the board after the move: version %d, last %d by %q, card %+v", b.Version, b.Last.ID, b.LastBy, b.Lanes[1][2])
	}
	if s := tab(t, e, ada); s.Op != "op1" || s.Refusal != "" || s.Lane != 1 || s.Pos != 2 || s.Version != 1 || s.Title != "Tune the tick" {
		t.Errorf("Ada's tab: %+v", s)
	}

	// Down its own lane, past the end: it goes last.
	e.Do(t, move(ada, "op2", 6, 1, 50, 0))
	if got, want := layout(t, e), [Lanes][]int64{{1, 2, 3, 5}, {7, 4, 8, 6}, {9, 10, 11, 12}}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after a move down its lane: %v", got)
	}
	// Up its own lane, to the top.
	e.Do(t, move(ada, "op3", 12, 2, 0, 0))
	if got, want := layout(t, e), [Lanes][]int64{{1, 2, 3, 5}, {7, 4, 8, 6}, {12, 9, 10, 11}}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after a move up its lane: %v", got)
	}
	// Every card out of a lane, and back into the empty one.
	for i, c := range []int64{1, 2, 3, 5} {
		e.Do(t, move(bob, fmt.Sprint("empty", i), c, 2, 0, 0))
	}
	e.Do(t, move(bob, "back", 1, 0, 3, 1))
	if got, want := layout(t, e), [Lanes][]int64{{1}, {7, 4, 8, 6}, {5, 3, 2, 12, 9, 10, 11}}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after emptying a lane and back: %v", got)
	}
}

// TestStale: two people move the same card from the same version. The
// first wins; the second is refused, and only its tab hears why.
func TestStale(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, move(ada, "a1", 7, 2, 0, 0))
	e.Touched()
	e.Do(t, move(bob, "b1", 7, 0, 0, 0))
	if got := e.Touched(); !slices.Equal(got, []string{"tab:board:tab-bob"}) {
		t.Errorf("a refused move touched %v", got)
	}
	if got := layout(t, e); got[2][0] != 7 {
		t.Errorf("the refused move moved the card: %v", got)
	}
	s := tab(t, e, bob)
	if s.Op != "b1" || s.Lane != 2 || s.Pos != 0 || s.Version != 1 ||
		!strings.Contains(s.Refusal, AvatarOf(WhoOf(ada.Session)).Name+" moved “Compress the frames” first. It is in Done now.") {
		t.Errorf("Bob's tab: %+v", s)
	}
	if s := tab(t, e, ada); s.Op != "a1" || s.Refusal != "" {
		t.Errorf("Ada's tab after Bob's refusal: %+v", s)
	}

	// The same person, in another tab of the same session.
	other := demo.Viewer{Session: ada.Session, Tab: "tab-ada-2"}
	e.Do(t, move(other, "a2", 7, 1, 0, 0))
	if s := tab(t, e, other); !strings.HasPrefix(s.Refusal, "You moved “Compress the frames” in another tab first.") {
		t.Errorf("a stale move from the mover's other tab: %+v", s)
	}

	// With the version it shows now, Bob's move goes through.
	e.Do(t, move(bob, "b2", 7, 0, 0, 1))
	if s := tab(t, e, bob); s.Refusal != "" || layout(t, e)[0][0] != 7 {
		t.Errorf("Bob's second move: %+v", s)
	}
}

func TestMoveInPlace(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, move(ada, "op1", 2, 0, 1, 0))
	if got := e.Touched(); !slices.Equal(got, []string{"tab:board:tab-ada"}) {
		t.Errorf("a move to where the card is touched %v", got)
	}
	if b := read(t, e); b.Version != 0 || b.Lanes[0][1].Version != 0 {
		t.Errorf("a move in place changed the board: %+v", b)
	}
	if s := tab(t, e, ada); s.Op != "op1" || s.Refusal != "" || s.Version != 0 {
		t.Errorf("Ada's tab: %+v", s)
	}
}

func TestMoveInvalid(t *testing.T) {
	e := demotest.Open(t, Demo)
	for name, m := range map[string]Move{
		"no tab":       {Viewer: demo.Viewer{Session: "s"}, Who: WhoOf("s"), Op: "o", Card: 1},
		"no op":        move(ada, "", 1, 0, 0, 0),
		"no mover":     {Viewer: ada, Op: "o", Card: 1},
		"no card":      move(ada, "o", 0, 0, 0, 0),
		"lane":         move(ada, "o", 1, 3, 0, 0),
		"no lane":      move(ada, "o", 1, -1, 0, 0),
		"index":        move(ada, "o", 1, 0, -1, 0),
		"far index":    move(ada, "o", 1, 0, maxIndex+1, 0),
		"version":      move(ada, "o", 1, 0, 0, -1),
		"mover format": {Viewer: ada, Who: "zz", Op: "o", Card: 1},
	} {
		if err := e.W.Do(context.Background(), m); !errors.Is(err, writer.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A card that does not exist is a refusal, not an error.
	e.Do(t, move(ada, "ghost", 99, 0, 0, 0))
	if s := tab(t, e, ada); s.Refusal != "That card is not on the board." {
		t.Errorf("a move of no card: %+v", s)
	}
}

func TestHello(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, Hello{Viewer: ada, Who: WhoOf(ada.Session), At: at})
	if got := e.Touched(); !slices.Equal(got, []string{HereTopic}) {
		t.Errorf("a new tab's hello touched %v", got)
	}
	e.Do(t, Hello{Viewer: ada, Who: WhoOf(ada.Session), At: at.Add(time.Minute)})
	if got := e.Touched(); len(got) != 0 {
		t.Errorf("a hello again touched %v", got)
	}
	if err := e.W.Do(context.Background(), Hello{Viewer: ada, Who: "nope"}); !errors.Is(err, writer.ErrInvalid) {
		t.Errorf("a hello without a session: %v", err)
	}
	e.Do(t, Hello{Viewer: bob, Who: WhoOf(bob.Session), At: at.Add(time.Second)})
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		people, err := present(ctx, tx, func(tab string) bool { return tab != "gone" })
		if err != nil {
			return err
		}
		if len(people) != 2 || people[0].Who != WhoOf(ada.Session) || people[1].Who != WhoOf(bob.Session) {
			t.Errorf("present: %+v", people)
		}
		people, err = present(ctx, tx, func(tab string) bool { return tab == bob.Tab })
		if err != nil {
			return err
		}
		if len(people) != 1 || people[0].Who != WhoOf(bob.Session) {
			t.Errorf("present with Ada's stream closed: %+v", people)
		}
		return nil
	})
	// Two tabs of one session are one person.
	e.Do(t, Hello{Viewer: demo.Viewer{Session: ada.Session, Tab: "tab-ada-2"}, Who: WhoOf(ada.Session), At: at.Add(2 * time.Second)})
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		people, err := present(ctx, tx, func(string) bool { return true })
		if err == nil && (len(people) != 2 || people[0].Tabs != 2 || hereLine(people) != "2 people, 3 tabs") {
			t.Errorf("two tabs of one session: %+v, %q", people, hereLine(people))
		}
		return err
	})
}

func TestReset(t *testing.T) {
	e := demotest.Open(t, Demo)
	// The last cards of to do and doing to the end of done: the others
	// stay where they started.
	e.Do(t, move(ada, "op1", 5, 2, 9, 0))
	e.Do(t, move(ada, "op2", 8, 2, 9, 0))
	e.Touched()
	sweepAt := func(now time.Time) {
		t.Helper()
		e.Do(t, writer.Func(func(tx *writer.Tx) error { return resetIfQuiet(tx, now) }))
	}
	sweepAt(at.Add(Quiet - time.Minute))
	if got := e.Touched(); len(got) != 0 || len(layout(t, e)[2]) != 6 {
		t.Fatalf("a sweep before a quiet hour reset the board: %v", got)
	}
	sweepAt(at.Add(Quiet))
	if got := e.Touched(); !slices.Equal(got, []string{"board"}) {
		t.Errorf("the reset touched %v", got)
	}
	if got, want := layout(t, e), [Lanes][]int64{{1, 2, 3, 4, 5}, {6, 7, 8}, {9, 10, 11, 12}}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after the reset: %v", got)
	}
	b := read(t, e)
	if b.Version != 3 || b.Last.ID != 0 || b.LastBy != "" || !b.MovedAt.IsZero() {
		t.Errorf("the board after the reset: version %d, last %d by %q at %v", b.Version, b.Last.ID, b.LastBy, b.MovedAt)
	}
	// The cards the reset moved have a new version, so a page that still
	// shows the old board cannot move them, and hears why. The others keep
	// theirs.
	for _, c := range append(append(b.Lanes[0], b.Lanes[1]...), b.Lanes[2]...) {
		want := int64(0)
		if c.ID == 5 || c.ID == 8 {
			want = 2
		}
		if c.Version != want || c.MovedBy != "" {
			t.Errorf("card %d after the reset: version %d, moved by %q", c.ID, c.Version, c.MovedBy)
		}
	}
	e.Do(t, move(bob, "late", 5, 1, 0, 1))
	if s := tab(t, e, bob); !strings.HasPrefix(s.Refusal, "The board went back to its start before your move arrived.") {
		t.Errorf("a move from before the reset: %+v", s)
	}
	// A quiet board that has been reset stays as it is.
	e.Touched()
	sweepAt(at.Add(3 * Quiet))
	if got := e.Touched(); len(got) != 0 {
		t.Errorf("a sweep of a board at its start touched %v", got)
	}
}

func TestForgetGone(t *testing.T) {
	e := demotest.Open(t, Demo)
	for i, v := range []demo.Viewer{ada, bob, {Session: "s3", Tab: "tab-3"}} {
		e.Do(t, Hello{Viewer: v, Who: WhoOf(v.Session), At: at.Add(time.Duration(i) * time.Minute)})
	}
	e.Touched()
	open := func(tab string) bool { return tab == bob.Tab }
	e.Do(t, writer.Func(func(tx *writer.Tx) error { return forgetGone(tx, at.Add(helloGrace+90*time.Second), open) }))
	var tabs []string
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT tab FROM board_here ORDER BY tab`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tab string
			rows.Scan(&tab)
			tabs = append(tabs, tab)
		}
		return rows.Err()
	})
	// Ada's is old and closed: gone. Bob's is old and open, the third's
	// closed but recent: kept.
	if !slices.Equal(tabs, []string{"tab-3", "tab-bob"}) {
		t.Errorf("after the sweep: %v", tabs)
	}
	// The demo's own sweep runs without a hub: nobody's stream is open.
	e.Do(t, writer.Func(func(tx *writer.Tx) error { return sweep(tx, at.Add(24*time.Hour)) }))
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		var n int
		err := tx.QueryRowContext(ctx, `SELECT count(*) FROM board_here`).Scan(&n)
		if err == nil && n != 0 {
			t.Errorf("%d hellos left a day later", n)
		}
		return err
	})
}

// TestViews renders the views as a tab's stream gets them: the board as a
// frame whose later form animates, with the board's version; the tab's
// outcome with its op id; the numbers; who is here.
func TestViews(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, move(ada, "op1", 4, 1, 0, 0))
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		vs := pageViews(ada)
		keys := []string{}
		for _, v := range vs {
			keys = append(keys, v.Key)
		}
		if !slices.Equal(keys, []string{"board:board", "board:stats", "board:here", "tab:board:tab-ada"}) {
			t.Fatalf("views: %v", keys)
		}
		f, err := vs[0].Render(ctx, tx)
		if err != nil {
			return err
		}
		frame, ok := f.(demo.Frame)
		if !ok || strings.Contains(string(frame.First), "useViewTransition") || !strings.Contains(string(frame.Next), "useViewTransition true") {
			t.Fatalf("the board's frame: %T", f)
		}
		first := string(frame.First)
		for _, want := range []string{
			`id="board-lanes"`,
			`id="board-card-4" class="bd-slot" data-card="4" data-version="1"`,
			`view-transition-name: board-card-4; view-transition-class: board-card board-moved;`,
			`view-transition-name: board-card-1;"`,
			`"_board":{"v":1}`,
			"moved “Tune the tick” to Doing.",
		} {
			if !strings.Contains(first, want) {
				t.Errorf("the board's frame has no %s:\n%s", want, first)
			}
		}
		if strings.Contains(first, ada.Session) || strings.Contains(first, ada.Tab) {
			t.Error("the shared board names its viewer")
		}

		out, err := vs[3].Render(ctx, tx)
		if err != nil {
			return err
		}
		b := string(out.([]byte))
		if !strings.Contains(b, "You moved “Tune the tick” to Doing.") || !strings.Contains(b, `"_board":{"ack":"op1","ackv":1,"ok":true}`) {
			t.Errorf("the tab's view:\n%s", b)
		}

		out, err = vs[1].Render(ctx, tx)
		if err != nil {
			return err
		}
		if b := string(out.([]byte)); !strings.Contains(b, `id="board-stats"`) || !strings.Contains(b, "<sb-sparkline") {
			t.Errorf("the numbers:\n%s", b)
		}

		page, err := renderPage(ctx, tx, ada)
		if err != nil {
			return err
		}
		var html strings.Builder
		page.Render(ctx, &html)
		for _, want := range []string{
			`data-signals="{_board: {v: 1,`,
			`id="board-here"`,
			`id="board-outcome"`,
			`[data-who=&#34;` + WhoOf(ada.Session),
		} {
			if !strings.Contains(html.String(), want) && !strings.Contains(html.String(), strings.ReplaceAll(want, "&#34;", `"`)) {
				t.Errorf("the page has no %s", want)
			}
		}
		embed, err := renderEmbed(ctx, tx, ada)
		if err != nil {
			return err
		}
		html.Reset()
		embed.Render(ctx, &html)
		if !strings.Contains(html.String(), `id="board-mini"`) || !strings.Contains(html.String(), `id="board-mini-4-1"`) || !strings.Contains(html.String(), `href="/demos/board"`) {
			t.Errorf("the embed:\n%s", html.String())
		}
		return nil
	})
}

func TestAvatar(t *testing.T) {
	a := AvatarOf(WhoOf("some session"))
	if a != AvatarOf(WhoOf("some session")) || a.Name == "" || a.Path == "" || len(a.Who) != 8 {
		t.Fatalf("an avatar: %+v", a)
	}
	if WhoOf("session-ada") == WhoOf("session-bob") {
		t.Error("two sessions share a seed")
	}
	// Every face is mirrored, and none is nearly empty.
	for i := range 2000 {
		a := AvatarOf(WhoOf(fmt.Sprint("s", i)))
		cells := map[[2]int]bool{}
		for _, m := range strings.Split(strings.TrimPrefix(a.Path, "M"), "M") {
			var x, y, w int
			fmt.Sscanf(m, "%d %dh%d", &x, &y, &w)
			for i := range w {
				cells[[2]int{x + i, y}] = true
			}
		}
		if len(cells) < 6 {
			t.Fatalf("%s: a face of %d pixels", a.Who, len(cells))
		}
		for c := range cells {
			if !cells[[2]int{6 - c[0], c[1]}] {
				t.Fatalf("%s: pixel %v has no mirror", a.Who, c)
			}
		}
		if a.Hue < 0 || a.Hue >= 360 {
			t.Fatalf("%s: hue %d", a.Who, a.Hue)
		}
	}
}

func TestTracker(t *testing.T) {
	var tr tracker
	now := time.Unix(1_800_000_000, 0)
	tr.rendered(now, 0, 3) // the first render: no moves known
	if n := tr.numbers(now, 3); n.Last.Moves != 0 {
		t.Errorf("a first render counts moves: %+v", n.Last)
	}
	tr.rendered(now.Add(time.Second), 1, 3)
	if n := tr.numbers(now.Add(time.Second), 3); n.Last.Moves != 1 || n.Last.Frames != 3 {
		t.Errorf("the first move after a start: %+v", n.Last)
	}
	tr.rendered(now.Add(2*time.Second), 4, 4) // three moves in one tick: one render
	n := tr.numbers(now.Add(3*time.Second), 4)
	if n.Last.Moves != 3 || n.Last.Frames != 4 || n.Viewers != 4 {
		t.Errorf("the last tick: %+v", n)
	}
	if n.PerSecond != float64(3+3+4)/rateOver || n.Series[seriesOf-1] != 4 || n.Series[seriesOf-2] != 3 || n.Series[seriesOf-3] != 3 {
		t.Errorf("the rate: %v, series %v", n.PerSecond, n.Series)
	}
	// A minute later the counts are gone, the last tick stays.
	n = tr.numbers(now.Add(2*time.Minute), 4)
	if n.PerSecond != 0 || slices.Max(n.Series) != 0 || n.Last.Moves != 3 {
		t.Errorf("a minute later: %+v", n)
	}
}

func TestFormat(t *testing.T) {
	for in, want := range map[int]int{0: 5, 4: 5, 5: 5, 6: 10, 11: 20, 199: 200, 201: 500} {
		if got := ceiling([]int{0, in}); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("ceiling(%d) = %d, want %d", in, got, want)
		}
	}
	if got := series([]int{0, 2, 10}); got != "[0,2,10]" {
		t.Errorf("series: %s", got)
	}
}
