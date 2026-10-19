package checkboxes

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demo/demotest"
)

var me = demo.Viewer{Session: "s1", Tab: "t1"}

// hub records what the commands ask of lith's hub.
type hub struct {
	mu      sync.Mutex
	watches map[string]map[string]bool // by stream, the keys it watches
	calls   int
}

func newHub() *hub { return &hub{watches: map[string]map[string]bool{}} }

func (h *hub) Watch(id string, v live.View) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if h.watches[id] == nil {
		h.watches[id] = map[string]bool{}
	}
	h.watches[id][v.Key] = true
	return true
}

func (h *hub) Unwatch(id, key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	ok := h.watches[id][key]
	delete(h.watches[id], key)
	return ok
}

// chunksOf returns the chunks a stream watches, in order.
func (h *hub) chunksOf(id string) []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []int
	for key := range h.watches[id] {
		n, err := strconv.Atoi(strings.TrimPrefix(key, Slug+":chunk:"))
		if err == nil {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

func span(from, to int) []int {
	var out []int
	for n := from; n < to; n++ {
		out = append(out, n)
	}
	return out
}

func TestWindowFor(t *testing.T) {
	for _, c := range []struct{ offset, count, cols, start int }{
		{0, 5, 1, 0},              // the top: nothing to centre above
		{100, 4, 1, 96},           // four rows asked, eight to spare: four above
		{100, 8, 2, 98},           // two to a row: rows of the window, 100 is in the middle
		{101, 8, 2, 98},           // an offset inside a row starts at the row
		{3900, 4, 1, Chunks - 12}, // the end: the window stays inside the grid
		{3906, 2, 2, 3896},        // two to a row: the last row has chunk 3906 alone
		{500, 60, 2, 524},         // more than a window asked: the middle of it
		{10, 5, 9, 8},             // more columns than the grid takes: four
	} {
		w := windowFor(c.offset, c.count, c.cols)
		if w.Start != c.start {
			t.Errorf("windowFor(%d, %d, %d) starts at %d, want %d", c.offset, c.count, c.cols, w.Start, c.start)
		}
	}
	end := Window{Start: 3896}
	if end.End() != Chunks || !slices.Equal(parked(end), []int{slot(3907)}) {
		t.Errorf("the last window: %d to %d, parked %v", end.Start, end.End(), parked(end))
	}
	if got := parked(Window{Start: 40}); got != nil {
		t.Errorf("a full window parks %v", got)
	}
	// Every chunk of a window has a slot of its own.
	for _, start := range []int{0, 7, 3895, 3896} {
		w, seen := Window{Start: start}, map[int]bool{}
		for n := w.Start; n < w.End(); n++ {
			seen[slot(n)] = true
		}
		for _, s := range parked(w) {
			seen[s] = true
		}
		if len(seen) != Slots {
			t.Errorf("window %d: %d slots", start, len(seen))
		}
	}
}

func TestGrouped(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 999_937: "999,937", 1_000_000: "1,000,000", -1234: "-1,234"} {
		if got := grouped(n); got != want {
			t.Errorf("grouped(%d) = %q", n, got)
		}
	}
}

func TestSetBox(t *testing.T) {
	e := demotest.Open(t, Demo)
	at := time.Unix(1_800_000_000, 0)
	e.Do(t, SetBox{Box: 0, Checked: true, At: at})
	e.Do(t, SetBox{Box: 999_999, Checked: true, At: at})
	if got := e.Touched(); !slices.Equal(got, []string{"checkboxes:chunk:0", "checkboxes:chunk:3906"}) {
		t.Errorf("two flips touched %v", got)
	}
	bits := read(t, e, 0)
	if !isSet(bits, 0) || isSet(bits, 1) {
		t.Errorf("chunk 0 after box 1: % x", bits)
	}
	if last := read(t, e, Chunks-1); !isSet(last, 63) || isSet(last, 62) {
		t.Errorf("the last chunk after the last box: % x", last)
	}

	// The same state again: nothing flips, nothing renders.
	e.Do(t, SetBox{Box: 0, Checked: true, At: at})
	if got := e.Touched(); len(got) != 0 {
		t.Errorf("a box set as it was touched %v", got)
	}
	e.Do(t, SetBox{Box: 0, Checked: false, At: at.Add(time.Second)})
	s := readStats(t, e, at.Add(2*time.Second))
	if s.Checked != 1 || s.Flips != 3 || s.Rate[Trend-2] != 2 || s.Rate[Trend-1] != 1 || s.PerSecond() != 1 {
		t.Errorf("stats: %+v", s)
	}

	for _, box := range []int{-1, Boxes} {
		if err := e.W.Do(context.Background(), SetBox{Box: box, Checked: true, At: at}); !errors.Is(err, writer.ErrInvalid) {
			t.Errorf("box %d: %v", box, err)
		}
	}
}

// TestSetWindow moves a tab's window and checks that its stream watches
// the chunks of the newest one, and no others.
func TestSetWindow(t *testing.T) {
	e := demotest.Open(t, Demo)
	h := newHub()
	at := time.Unix(1_800_000_000, 0)

	// A new tab shows the top and answers its stream's first frame with
	// that window: nothing to store, and the stream gets the top's chunks.
	e.Do(t, SetWindow{Viewer: me, Window: Window{Start: 0}, At: at, Hub: h})
	if got := h.chunksOf(me.Tab); !slices.Equal(got, span(0, Slots)) {
		t.Errorf("a new tab watches %v", got)
	}
	if got := e.Touched(); len(got) != 0 {
		t.Errorf("the window a new tab shows touched %v", got)
	}

	e.Do(t, SetWindow{Viewer: me, Window: Window{Start: 6}, At: at, Hub: h})
	if got := h.chunksOf(me.Tab); !slices.Equal(got, span(6, 18)) {
		t.Errorf("after a scroll the tab watches %v", got)
	}
	if got := e.Touched(); !slices.Equal(got, []string{"tab:checkboxes:t1"}) {
		t.Errorf("a window touched %v", got)
	}
	e.Do(t, SetWindow{Viewer: me, Window: Window{Start: 2000}, At: at, Hub: h})
	if got := h.chunksOf(me.Tab); !slices.Equal(got, span(2000, 2012)) {
		t.Errorf("after a jump the tab watches %v", got)
	}
	e.Do(t, SetWindow{Viewer: me, Window: Window{Start: 3896}, At: at, Hub: h})
	if got := h.chunksOf(me.Tab); !slices.Equal(got, span(3896, Chunks)) {
		t.Errorf("at the end the tab watches %v", got)
	}
	e.Touched()

	// The same window again stores nothing; the watches stay.
	e.Do(t, SetWindow{Viewer: me, Window: Window{Start: 3896}, At: at, Hub: h})
	if got := e.Touched(); len(got) != 0 {
		t.Errorf("the same window touched %v", got)
	}

	// The stream comes back (a deploy): the page answers its first frame
	// with the window it shows, and the new stream watches it.
	back := newHub()
	e.Do(t, SetWindow{Viewer: me, Window: Window{Start: 3896}, At: at, Hub: back})
	if got := back.chunksOf(me.Tab); !slices.Equal(got, span(3896, Chunks)) {
		t.Errorf("a stream that comes back watches %v", got)
	}

	// Another tab's window is its own.
	other := demo.Viewer{Session: "s2", Tab: "t2"}
	e.Do(t, SetWindow{Viewer: other, Window: Window{Start: 12}, At: at, Hub: h})
	if got := h.chunksOf("t2"); !slices.Equal(got, span(12, 24)) {
		t.Errorf("the other tab watches %v", got)
	}
	if got := h.chunksOf(me.Tab); !slices.Equal(got, span(3896, Chunks)) {
		t.Errorf("the other tab moved this one's watches: %v", got)
	}

	if err := e.W.Do(context.Background(), SetWindow{Viewer: me, Window: Window{Start: Chunks}, Hub: h}); !errors.Is(err, writer.ErrInvalid) {
		t.Errorf("a window past the end: %v", err)
	}
}

// TestViews renders the views as a tab's stream gets them.
func TestViews(t *testing.T) {
	e := demotest.Open(t, Demo)
	h := newHub()
	at := time.Now()
	for _, box := range []int{256 * 13, 256*13 + 17, 256*13 + 255} {
		e.Do(t, SetBox{Box: box, Checked: true, At: at})
	}
	e.Do(t, SetWindow{Viewer: me, Window: Window{Start: 10}, At: at, Hub: h})
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		frame, err := chunkView(13).Render(ctx, tx)
		if err != nil {
			return err
		}
		f := string(frame.([]byte))
		for _, want := range []string{
			"data: selector #checkboxes-slot-1\n", "data: mode inner\n",
			`<div id="checkboxes-tile-13" class="cb-tile" role="listitem" aria-label="Boxes 3,329 to 3,584, 3 checked" data-chunk="13"`,
			`<input type=checkbox title=3,329 checked>`, `<input type=checkbox title=3,330>`, `<input type=checkbox title=3,346 checked>`, `<input type=checkbox title=3,584 checked>`,
			`<span class="cb-tile__on">3 checked</span>`,
		} {
			if !strings.Contains(f, want) {
				t.Errorf("chunk 13's frame has no %s", want)
			}
		}
		if n := strings.Count(f, "<input"); n != ChunkSize {
			t.Errorf("chunk 13's tile has %d boxes", n)
		}
		if strings.Contains(f, "watching") {
			t.Error("a tile one tab watches counts its watchers")
		}
		if two := string(tileFrame(13, make([]byte, 32), 2)); !strings.Contains(two, `<span class="cb-tile__eyes">2 watching</span>`) {
			t.Error("a tile two tabs watch does not say so")
		}
		last, err := chunkView(Chunks-1).Render(ctx, tx)
		if err != nil {
			return err
		}
		if l := string(last.([]byte)); strings.Count(l, "<input") != 64 || !strings.Contains(l, "title=1,000,000>") {
			t.Errorf("the last tile:\n%s", l)
		}

		board, err := boardView(me).Render(ctx, tx)
		if err != nil {
			return err
		}
		frames := board.(demo.Frame)
		b := string(frames.Next)
		if first := string(frames.First); !strings.HasPrefix(first, b) || !strings.Contains(first, `{"_checkboxes":{"hello":true}}`) || strings.Contains(b, "datastar-patch-signals") {
			t.Errorf("only the list's first frame asks for the window:\n%s", first)
		}
		if !strings.Contains(b, `offset="10"`) || !strings.Contains(b, `total="3907"`) {
			t.Errorf("the tab's list:\n%s", b)
		}
		order := regexp.MustCompile(`id="checkboxes-slot-(\d+)"`).FindAllStringSubmatch(b, -1)
		var slots []string
		for _, m := range order {
			slots = append(slots, m[1])
		}
		if strings.Join(slots, " ") != "10 11 0 1 2 3 4 5 6 7 8 9" {
			t.Errorf("the slots in the order of window 10: %v", slots)
		}
		if strings.Contains(b, "<input") {
			t.Error("the tab's view carries tiles")
		}

		stats, err := statsView().Render(ctx, tx)
		if err != nil {
			return err
		}
		if s := string(stats.([]byte)); !strings.Contains(s, `<dd>3</dd>`) || !strings.Contains(s, `values="[`) {
			t.Errorf("the numbers:\n%s", s)
		}

		page, err := renderPage(ctx, tx, me)
		if err != nil {
			return err
		}
		var html strings.Builder
		page.Render(ctx, &html)
		if p := html.String(); !strings.Contains(p, `id="checkboxes-tile-13"`) || strings.Count(p, "<input type=checkbox") != Slots*ChunkSize {
			t.Errorf("the page has %d boxes", strings.Count(p, "<input type=checkbox"))
		}
		return nil
	})
}

func TestSweep(t *testing.T) {
	e := demotest.Open(t, Demo)
	now := time.Unix(1_800_000_000, 0)
	for _, ago := range []time.Duration{2 * time.Hour, 61 * time.Minute, 59 * time.Minute, time.Second} {
		e.Do(t, SetBox{Box: int(ago / time.Second % Boxes), Checked: true, At: now.Add(-ago)})
	}
	e.Do(t, writer.Func(func(tx *writer.Tx) error { return sweep(tx, now) }))
	var n int
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM checkboxes_seconds`).Scan(&n)
	})
	if n != 2 {
		t.Errorf("the sweep kept %d seconds", n)
	}
}

func TestExpressions(t *testing.T) {
	for name, x := range map[string]string{"flip": flip, "jump": jump, "focusTarget": focusTarget, "arrows": arrows} {
		if strings.Contains(x, "CHUNK") || strings.Contains(x, "BOXES") || strings.Contains(x, "GROUPED") {
			t.Errorf("%s has a placeholder left", name)
		}
		// Datastar reads $a.b-c as one path: a signal is never followed by
		// a minus without a space.
		if m := regexp.MustCompile(`\$[\w.]+-`).FindString(x); m != "" {
			t.Errorf("%s: %q", name, m)
		}
	}
	if !strings.Contains(flip, "* 256 +") || !strings.Contains(jump, "1,000,000") {
		t.Error("the numbers are not filled in")
	}
}

func read(t *testing.T, e *demotest.Env, n int) (bits []byte) {
	t.Helper()
	e.View(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		bits, err = chunk(ctx, tx, n)
		return err
	})
	return bits
}

func readStats(t *testing.T, e *demotest.Env, now time.Time) (s Stats) {
	t.Helper()
	e.View(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		s, err = stats(ctx, tx, now)
		return err
	})
	return s
}
