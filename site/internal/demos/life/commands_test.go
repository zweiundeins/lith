package life

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demo/demotest"
)

var (
	ada = demo.Viewer{Session: "s-ada", Tab: "t-ada"}
	bob = demo.Viewer{Session: "s-bob", Tab: "t-bob"}
	t0  = time.UnixMilli(1_800_000_000_000)
)

func board(t *testing.T, e *demotest.Env) (b Board) {
	t.Helper()
	e.View(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		b, err = load(ctx, tx)
		return err
	})
	return b
}

func tabState(t *testing.T, e *demotest.Env, v demo.Viewer) (s TabState) {
	t.Helper()
	e.View(t, func(ctx context.Context, tx *sql.Tx) error { return demo.LoadTab(ctx, tx, Slug, v, &s) })
	return s
}

// set replaces the board's cells.
func set(t *testing.T, e *demotest.Env, cells []byte) {
	t.Helper()
	e.Do(t, writer.Func(func(tx *writer.Tx) error {
		b, err := load(tx.Context(), tx)
		if err != nil {
			return err
		}
		b.Cells = cells
		b.changed(t0.UnixMilli())
		return b.save(tx)
	}))
	e.Touched()
}

// The first step seeds the empty board; the next ones advance it and
// count the rate.
func TestStep(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, Step{At: t0})
	b := board(t, e)
	if b.Generation != 0 || Population(b.Cells) < Cells/4 || b.Quiet != 0 {
		t.Fatalf("the first step: generation %d, %d cells, quiet %d", b.Generation, Population(b.Cells), b.Quiet)
	}
	if got := e.Touched(); !slices.Equal(got, []string{"life"}) {
		t.Errorf("a step touched %v", got)
	}
	for i := 1; i <= 25; i++ {
		e.Do(t, Step{At: t0.Add(time.Duration(i) * 100 * time.Millisecond)})
	}
	b = board(t, e)
	if b.Generation != 25 || b.Rate != 100 || b.History == "[]" {
		t.Errorf("25 steps in 2.5 s: generation %d, rate %s, history %s", b.Generation, tenths(b.Rate), b.History)
	}
	// After a pause, the rate counts again from the next step.
	e.Do(t, Step{At: t0.Add(time.Minute)})
	if b = board(t, e); b.Generation != 26 || b.RateSince != t0.Add(time.Minute).UnixMilli() {
		t.Errorf("after a pause: generation %d, rate since %d", b.Generation, b.RateSince)
	}
}

// An empty board stays as it is, and gets a new soup after EmptyFor.
func TestEmptyBoard(t *testing.T) {
	e := demotest.Open(t, Demo)
	set(t, e, make([]byte, Cells))
	e.Do(t, Step{At: t0.Add(time.Second)})
	b := board(t, e)
	if b.Generation != 0 || b.Quiet != t0.UnixMilli() {
		t.Fatalf("an empty board: generation %d, quiet %d", b.Generation, b.Quiet)
	}
	if got := e.Touched(); len(got) != 0 {
		t.Errorf("a step on an empty board touched %v", got)
	}
	e.Do(t, Step{At: t0.Add(EmptyFor)})
	if b = board(t, e); Population(b.Cells) == 0 || b.Quiet != 0 || b.Generation != 0 {
		t.Errorf("after %v: %d cells, quiet %d, generation %d", EmptyFor, Population(b.Cells), b.Quiet, b.Generation)
	}
	// The new soup counts its rate from its own first step.
	for i := 1; i <= 11; i++ {
		e.Do(t, Step{At: t0.Add(EmptyFor + time.Duration(i)*100*time.Millisecond)})
	}
	if b = board(t, e); b.Rate != 100 || b.Generation != 11 {
		t.Errorf("the new soup's rate: %s at generation %d", tenths(b.Rate), b.Generation)
	}
}

// A board that repeats itself, a block and a blinker, keeps blinking and
// gets a new soup after SettledFor; a drawing in between wakes it.
func TestSettledBoard(t *testing.T) {
	e := demotest.Open(t, Demo)
	cells := place(10, 10, 1, "OO", "OO")
	cells[at(30, 30)], cells[at(31, 30)], cells[at(32, 30)] = 2, 2, 2
	set(t, e, cells)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	for i := 1; i <= 3; i++ {
		e.Do(t, Step{At: at(100 * i)})
	}
	b := board(t, e)
	if b.Quiet != at(200).UnixMilli() || b.Generation != 3 {
		t.Fatalf("a block and a blinker: quiet %d, generation %d", b.Quiet, b.Generation)
	}
	e.Do(t, Draw{Viewer: ada, Op: "op1", Cells: []int{5}, Alive: true, At: at(400)})
	if b = board(t, e); b.Quiet != 0 {
		t.Fatalf("a drawing leaves the board quiet since %d", b.Quiet)
	}
	e.Do(t, Step{At: at(500)})
	e.Do(t, Step{At: at(600)})
	e.Do(t, Step{At: at(700)})
	quiet := board(t, e).Quiet
	e.Do(t, Step{At: time.UnixMilli(quiet).Add(SettledFor - time.Millisecond)})
	if b = board(t, e); b.Generation == 0 {
		t.Fatal("reseeded before its time")
	}
	e.Do(t, Step{At: time.UnixMilli(quiet).Add(SettledFor)})
	if b = board(t, e); b.Generation != 0 || Population(b.Cells) < Cells/4 {
		t.Errorf("after %v: generation %d, %d cells", SettledFor, b.Generation, Population(b.Cells))
	}
}

// A stroke draws lines between its points in the session's colour, and a
// stroke that clears kills them; the tab hears its op.
func TestDraw(t *testing.T) {
	e := demotest.Open(t, Demo)
	set(t, e, make([]byte, Cells))
	e.Do(t, Draw{Viewer: ada, Op: "op1", Cells: []int{at(10, 10), at(14, 10), at(14, 12)}, Alive: true, At: t0})
	b := board(t, e)
	colour := byte(defaultColour(ada.Session))
	if Population(b.Cells) != 7 || b.Cells[at(12, 10)] != colour || b.Cells[at(14, 11)] != colour || b.Quiet != 0 {
		t.Errorf("a stroke drew %d cells:\n%s", Population(b.Cells), Plain(b.Cells))
	}
	if got := e.Touched(); !slices.Equal(got, []string{"life", "tab:life:t-ada"}) {
		t.Errorf("a stroke touched %v", got)
	}
	if s := tabState(t, e, ada); s.Op != "op1" || s.Refusal != "" {
		t.Errorf("the tab's state: %+v", s)
	}
	e.Do(t, Draw{Viewer: ada, Op: "op2", Cells: []int{at(10, 10), at(14, 10)}, Alive: false, At: t0})
	if b = board(t, e); Population(b.Cells) != 2 {
		t.Errorf("a clearing stroke left %d cells", Population(b.Cells))
	}
	for _, bad := range []Draw{
		{Viewer: ada, Op: "op3", Alive: true},
		{Viewer: ada, Op: "op3", Cells: []int{Cells}, Alive: true},
		{Viewer: ada, Cells: []int{1}, Alive: true},
		{Viewer: ada, Op: "op3", Cells: make([]int, MaxStroke+1), Alive: true},
	} {
		if err := e.W.Do(context.Background(), bad); !errors.Is(err, writer.ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

// A pattern lands in the colour its session picked, turned as asked.
func TestDropAndPick(t *testing.T) {
	e := demotest.Open(t, Demo)
	set(t, e, make([]byte, Cells))
	e.Do(t, Pick{Viewer: bob, Colour: 7, At: t0})
	if got := e.Touched(); !slices.Equal(got, []string{"sid:life:s-bob"}) {
		t.Errorf("a pick touched %v", got)
	}
	e.Do(t, Drop{Viewer: bob, Op: "op1", Pattern: "glider", Cell: at(40, 25), Turn: 1, At: t0})
	want := make([]byte, Cells)
	g, _ := Find("glider")
	g.Stamp(want, at(40, 25), 1, 7)
	same(t, "the dropped glider", board(t, e).Cells, want)
	if got := e.Touched(); !slices.Equal(got, []string{"life", "tab:life:t-bob"}) {
		t.Errorf("a drop touched %v", got)
	}
	for _, bad := range []writer.Command{
		Drop{Viewer: bob, Op: "op2", Pattern: "cannon", Cell: 1},
		Drop{Viewer: bob, Op: "op2", Pattern: "glider", Cell: -1},
		Drop{Viewer: bob, Op: "op2", Pattern: "glider", Cell: 1, Turn: 4},
		Pick{Viewer: bob, Colour: 9},
	} {
		if err := e.W.Do(context.Background(), bad); !errors.Is(err, writer.ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

// A reset clears the board; another within ResetEvery, or one of an empty
// board, is refused, and only the tab that asked hears why.
func TestReset(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, Step{At: t0})
	e.Touched()
	e.Do(t, Reset{Viewer: ada, Op: "op1", At: t0})
	b := board(t, e)
	if Population(b.Cells) != 0 || b.Generation != 0 || b.Reset != t0.UnixMilli() || b.Quiet != t0.UnixMilli() {
		t.Fatalf("after a reset: %d cells, generation %d, reset %d, quiet %d", Population(b.Cells), b.Generation, b.Reset, b.Quiet)
	}
	if got := e.Touched(); !slices.Equal(got, []string{"life", "tab:life:t-ada"}) {
		t.Errorf("a reset touched %v", got)
	}

	e.Do(t, Reset{Viewer: bob, Op: "op2", At: t0.Add(time.Second)})
	if s := tabState(t, e, bob); s.Op != "op2" || !strings.Contains(s.Refusal, "empty already") {
		t.Errorf("a reset of an empty board: %+v", s)
	}
	if got := e.Touched(); !slices.Equal(got, []string{"tab:life:t-bob"}) {
		t.Errorf("a refusal touched %v", got)
	}

	e.Do(t, Draw{Viewer: bob, Op: "op3", Cells: []int{1, 2, 3}, Alive: true, At: t0.Add(2 * time.Second)})
	e.Do(t, Reset{Viewer: bob, Op: "op4", At: t0.Add(4 * time.Second)})
	if s := tabState(t, e, bob); !strings.Contains(s.Refusal, "Someone reset the board 4 s ago. Try again in 6 s.") {
		t.Errorf("a reset too soon: %+v", s)
	}
	if ago(1400*time.Millisecond)+" "+seconds(ResetEvery-1400*time.Millisecond) != "1 s ago 9 s" || ago(300*time.Millisecond) != "just now" {
		t.Errorf("ago and seconds: %s, %s", ago(1400*time.Millisecond), ago(300*time.Millisecond))
	}
	if Population(board(t, e).Cells) != 3 {
		t.Error("a refused reset cleared the board")
	}
	e.Do(t, Reset{Viewer: bob, Op: "op5", At: t0.Add(ResetEvery)})
	if s := tabState(t, e, bob); s.Refusal != "" || Population(board(t, e).Cells) != 0 {
		t.Errorf("a reset in its time: %+v", s)
	}
}

func TestSweep(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, Pick{Viewer: ada, Colour: 3, At: t0.Add(-PlayerTTL - time.Hour)})
	e.Do(t, Pick{Viewer: bob, Colour: 4, At: t0})
	e.Do(t, writer.Func(func(tx *writer.Tx) error { return sweep(tx, t0) }))
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		a, err := colourOf(ctx, tx, ada.Session)
		if err != nil {
			return err
		}
		b, err := colourOf(ctx, tx, bob.Session)
		if a != defaultColour(ada.Session) || b != 4 {
			t.Errorf("after the sweep: ada %d, bob %d", a, b)
		}
		return err
	})
}

// The board's view is one patch of the board, the counters, the shares
// and the status, on one line, every cell the same length; the tab's view
// is its outcome and the signals that end its pending state.
func TestViews(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, Step{At: t0})
	e.Do(t, Pick{Viewer: ada, Colour: 5, At: t0})
	e.Do(t, Draw{Viewer: ada, Op: "op1", Cells: []int{0}, Alive: true, At: t0})
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		vs := pageViews(ada)
		if len(vs) != 2 || vs[0].Key != BoardKey || !vs[0].Presence || vs[1].Key != "tab:life:t-ada" || !slices.Contains(vs[1].Topics, "sid:life:s-ada") {
			t.Fatalf("views: %+v", vs)
		}
		out, err := vs[0].Render(ctx, tx)
		if err != nil {
			return err
		}
		frame := string(out.([]byte))
		lines := strings.Count(frame, "\ndata: elements ")
		if lines != 1 || strings.Count(frame, "<p class=") != Cells || !strings.Contains(frame, `<p class=e><p class=_>`) {
			t.Errorf("the frame has %d lines and %d cells", lines, strings.Count(frame, "<p class="))
		}
		for _, want := range []string{`<div id="life-board"`, `id="life-stats"`, `id="life-share"`, `id="life-status"`, `<dt>Here now</dt><dd>0</dd>`, `values="[]"`} {
			if !strings.Contains(frame, want) {
				t.Errorf("the frame has no %s", want)
			}
		}
		if strings.Index(frame, `id="life-board"`) > strings.Index(frame, `id="life-stats"`) {
			t.Error("the board does not come first in its frame")
		}
		out, err = vs[1].Render(ctx, tx)
		if err != nil {
			return err
		}
		if tab := string(out.([]byte)); !strings.Contains(tab, `id="life-outcome"`) || !strings.Contains(tab, `"_life":{"ack":"op1","colour":5}`) {
			t.Errorf("the tab's view:\n%s", tab)
		}
		page, err := renderPage(ctx, tx, ada)
		if err != nil {
			return err
		}
		var html strings.Builder
		if err := page.Render(ctx, &html); err != nil {
			return err
		}
		for _, want := range []string{`colour: 5, pick: 5`, `data-on:pointerdown=`, `id="life-board"`, `aria-pressed="true"`, `data-shape="gun"`} {
			if !strings.Contains(html.String(), want) {
				t.Errorf("the page has no %s", want)
			}
		}
		return nil
	})
}

func TestStatus(t *testing.T) {
	b := Board{Cells: make([]byte, Cells), Quiet: t0.UnixMilli()}
	if got := status(b, t0.Add(4*time.Second)); got != "The board is empty. A new soup in 11 s, unless someone draws." {
		t.Errorf("empty: %q", got)
	}
	b.Cells[0] = 1
	if got := status(b, t0.Add(SettledFor+time.Second)); !strings.Contains(got, "repeats itself. A new soup in 1 s") {
		t.Errorf("settled, overdue: %q", got)
	}
	if got := number(1234567); got != "1,234,567" {
		t.Errorf("number: %s", got)
	}
}

// The clock waits while nobody watches, and ends when the hub stops.
func TestClockEndsWithTheHub(t *testing.T) {
	e := demotest.Open(t, Demo)
	hub := live.New(live.Options{Read: e.DB.R, Log: slog.New(slog.DiscardHandler)})
	ctx, stop := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- hub.Run(ctx) }()
	done := make(chan struct{})
	go func() {
		(&clock{w: e.W, hub: hub, log: slog.New(slog.DiscardHandler)}).run()
		close(done)
	}()
	time.Sleep(3 * Idle)
	if b := board(t, e); b.Generation != 0 || Population(b.Cells) != 0 {
		t.Errorf("with nobody watching, the clock stepped to generation %d", b.Generation)
	}
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the clock outlived the hub")
	}
	if err := <-ran; err != nil {
		t.Fatal(err)
	}
}
