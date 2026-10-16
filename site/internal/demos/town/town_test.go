package town

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/events/eventstest"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demo/demotest"
)

var refusedOnWater = &writer.Refusal{Msg: "Only a road goes on water, as a bridge. Clear the tile first."}

// TestDecide runs the decider as given, when, then: a tile's events, a
// change, and the events it causes.
func TestDecide(t *testing.T) {
	grass := eventstest.For(PlaceTile)
	grass.When(Place{Kind: House}).Then(t, Built{Kind: House})
	grass.When(Place{Kind: Road}).Then(t, Built{Kind: Road})
	grass.When(Place{Kind: Grass}).Then(t)

	house := grass.Given(Built{Kind: House})
	house.When(Place{Kind: House}).Then(t)
	house.When(Place{Kind: Tree}).Then(t, Built{Kind: Tree, Over: House})
	house.When(Place{Kind: Water}).Then(t, Built{Kind: Water, Over: House})
	house.When(Place{Kind: Grass}).Then(t, Cleared{Was: House})
	house.Given(Cleared{Was: House}).When(Place{Kind: Grass}).Then(t)

	// A road on water is a bridge; nothing else stands in water.
	water := grass.Given(Built{Kind: Water})
	water.When(Place{Kind: Road}).Then(t, Built{Kind: Bridge, Over: Water})
	water.When(Place{Kind: House}).ThenError(t, refusedOnWater)
	water.When(Place{Kind: Tree}).ThenError(t, refusedOnWater)
	water.When(Place{Kind: Grass}).Then(t, Cleared{Was: Water})

	bridge := water.Given(Built{Kind: Bridge, Over: Water})
	bridge.When(Place{Kind: Road}).Then(t)
	bridge.When(Place{Kind: Water}).Then(t, Built{Kind: Water, Over: Bridge})
	bridge.When(Place{Kind: House}).ThenError(t, refusedOnWater)
	bridge.When(Place{Kind: Grass}).Then(t, Cleared{Was: Bridge})
}

// TestEventSamples decodes a sample of every event type as it was first
// stored.
func TestEventSamples(t *testing.T) {
	eventstest.Samples(t, Events, TileStream, os.DirFS("testdata/tile"))
}

var (
	ada = demo.Viewer{Session: "s-ada", Tab: "tab-ada"}
	bob = demo.Viewer{Session: "s-bob", Tab: "tab-bob"}
)

func change(v demo.Viewer, op string, tile int, kind string, seen int64) Place {
	return Place{Viewer: v, Who: whoOf(v.Session), Op: op, Tile: tile, Kind: kind, Seen: seen, At: time.Now()}
}

// TestPlace changes tiles through the commands: what they write, the
// topics they touch, a change from a page that saw an older tile, a
// refusal, and a tab in the past.
func TestPlace(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, change(ada, "op1", 5, House, 0))
	town, s := readTown(t, e, ada)
	if town[5] != (Tile{Kind: House, Who: whoOf(ada.Session), Version: 1}) || s.Op != "op1" || s.Refusal != "" {
		t.Fatalf("a house: %+v, tab %+v", town[5], s)
	}
	if got := e.Touched(); !slices.Equal(got, []string{"lith:events", "town", "town:tab:tab-ada"}) {
		t.Errorf("a change touched %v", got)
	}

	// Bob's page still showed grass: refused, and the tile stays.
	e.Do(t, change(bob, "op2", 5, Tree, 0))
	town, s = readTown(t, e, bob)
	if town[5].Kind != House || s.Refusal != nameOf(whoOf(ada.Session))+" changed F1 first. It is a house now." {
		t.Fatalf("a stale change: %+v, tab %+v", town[5], s)
	}
	if got := e.Touched(); !slices.Equal(got, []string{"town:tab:tab-bob"}) {
		t.Errorf("a refused change touched %v", got)
	}

	// Ada again, from another tab of her session.
	other := demo.Viewer{Session: ada.Session, Tab: "tab-ada-2"}
	e.Do(t, change(other, "op3", 5, Tree, 0))
	if _, s = readTown(t, e, other); s.Refusal != "You changed F1 in another tab first. It is a house now." {
		t.Errorf("her own tab: %q", s.Refusal)
	}

	// Water, then a house on it: the decider refuses.
	e.Do(t, change(bob, "op4", 6, Water, 0))
	e.Do(t, change(bob, "op5", 6, House, 1))
	if town, s = readTown(t, e, bob); town[6].Kind != Water || s.Refusal != refusedOnWater.Msg {
		t.Errorf("a house on water: %+v, tab %+v", town[6], s)
	}
	e.Do(t, change(bob, "op6", 6, Road, 1))
	if town, _ = readTown(t, e, bob); town[6] != (Tile{Kind: Bridge, Who: whoOf(bob.Session), Version: 2}) {
		t.Errorf("a road on water: %+v", town[6])
	}

	// A tab in the past builds nothing.
	e.Do(t, Seek{Viewer: bob, At: 1, Seq: 1, Now: time.Now()})
	e.Touched()
	e.Do(t, change(bob, "op7", 7, Tree, 0))
	if town, s = readTown(t, e, bob); town[7].Kind != Grass || !strings.Contains(s.Refusal, "past") || s.At != 1 {
		t.Errorf("a change from the past: %+v, tab %+v", town[7], s)
	}
	if got := e.Touched(); !slices.Equal(got, []string{"town:tab:tab-bob"}) {
		t.Errorf("a change from the past touched %v", got)
	}

	// The writer refuses what no page sends.
	for _, bad := range []Place{change(ada, "op8", Tiles, House, 0), change(ada, "op9", 1, Bridge, 0), change(ada, "", 1, House, 0)} {
		if err := e.W.Do(context.Background(), bad); !errors.Is(err, writer.ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

// hub records what the commands ask of the live hub.
type hub struct{ log []string }

func (h *hub) Watch(id string, v live.View) bool {
	h.log = append(h.log, "watch "+v.Key+" "+strings.Join(v.Topics, ","))
	return true
}

func (h *hub) Unwatch(id, key string) bool {
	h.log = append(h.log, "unwatch "+key)
	return true
}

func (h *hub) take() []string {
	l := h.log
	h.log = nil
	return l
}

// TestSeek moves a tab through the log: its stream leaves the views of
// one position for the next, the slider's end is now, a seek the network
// delayed changes nothing, and a stream's hello watches the tab's views.
func TestSeek(t *testing.T) {
	e := demotest.Open(t, Demo)
	for i := range 6 {
		e.Do(t, change(ada, "op"+string(rune('a'+i)), i, House, 0))
	}
	h := &hub{}
	e.Touched()
	e.Do(t, Seek{Viewer: bob, At: 2, Seq: 1, Now: time.Now(), Hub: h})
	if got := h.take(); !slices.Equal(got, []string{"unwatch town:now", "watch town:at:2 town:pruned", "watch town:later:2 town:pruned"}) {
		t.Errorf("to 2: %v", got)
	}
	if got := e.Touched(); !slices.Equal(got, []string{"town:tab:tab-bob"}) {
		t.Errorf("a seek touched %v", got)
	}
	// Two events after 4: the events after it can still grow.
	e.Do(t, Seek{Viewer: bob, At: 4, Seq: 3, Now: time.Now(), Hub: h})
	if got := h.take(); !slices.Equal(got, []string{"unwatch town:at:2", "unwatch town:later:2", "watch town:at:4 town:pruned", "watch town:later:4 town:pruned,town"}) {
		t.Errorf("to 4: %v", got)
	}
	// Seek 2, delayed past seek 3, changes nothing.
	e.Do(t, Seek{Viewer: bob, At: 1, Seq: 2, Now: time.Now(), Hub: h})
	if _, s := readTown(t, e, bob); s.At != 4 || s.Seq != 3 || len(h.take()) != 0 {
		t.Errorf("a late seek: tab %+v", s)
	}
	// The end of the slider, or beyond, is now.
	e.Do(t, Seek{Viewer: bob, At: 6, Seq: 4, Now: time.Now(), Hub: h})
	if _, s := readTown(t, e, bob); s.At != 0 || s.Seq != 4 {
		t.Errorf("to the end: tab %+v", s)
	}
	if got := h.take(); !slices.Equal(got, []string{"unwatch town:at:4", "unwatch town:later:4", "watch town:now town,town:pruned"}) {
		t.Errorf("to the end: %v", got)
	}
	// The same position again: only the seek's number changes.
	e.Do(t, Seek{Viewer: bob, At: 0, Seq: 5, Now: time.Now(), Hub: h})
	if _, s := readTown(t, e, bob); s.Seq != 5 || len(h.take()) != 0 {
		t.Errorf("now again: tab %+v", s)
	}
	// A new stream says hello: it watches the tab's position.
	e.Do(t, Seek{Viewer: bob, At: 3, Seq: 6, Now: time.Now(), Hub: h})
	h.take()
	e.Do(t, Follow{Viewer: bob, Hub: h})
	if got := h.take(); !slices.Equal(got, []string{"watch town:at:3 town:pruned", "watch town:later:3 town:pruned"}) {
		t.Errorf("hello: %v", got)
	}
}

// TestViews renders the views as a tab's stream gets them.
func TestViews(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, Seed(time.Now()))
	e.Do(t, change(ada, "op1", 0, House, 0))
	e.Do(t, Seek{Viewer: bob, At: 3, Seq: 1, Now: time.Now()})
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		vs := pageViews(bob)
		if len(vs) != 2 || vs[0].Key != "town:tab:tab-bob" || vs[1].Key != "town:head" {
			t.Fatalf("views: %v %v", vs[0].Key, vs[1].Key)
		}
		f, err := vs[0].Render(ctx, tx)
		if err != nil {
			return err
		}
		frame := f.(demo.Frame)
		if !strings.Contains(string(frame.First), `"hello":true`) || strings.Contains(string(frame.Next), "hello") ||
			!strings.Contains(string(frame.Next), `{"_town":{"at":3,"seq":1,"ack":"","ok":false}}`) {
			t.Errorf("the tab's view:\n%s", frame.First)
		}
		last := len(start) + 1 // the starting town and Ada's house
		head := render(t, ctx, tx, vs[1])
		if !strings.Contains(head, fmt.Sprintf(`{"_town":{"first":1,"head":%d}}`, last)) {
			t.Errorf("the head:\n%s", head)
		}
		now := render(t, ctx, tx, nowView())
		for _, want := range []string{`id="town-map" class="town-map" data-at="0"`, `data-t="0" data-k="house" data-v="1"`, `id="town-later"`, fmt.Sprint("position ", last), "built a house on A1"} {
			if !strings.Contains(now, want) {
				t.Errorf("the town now has no %s", want)
			}
		}
		// The town as of 3: the river's first three tiles, nothing to
		// build on, and no events after it.
		at := render(t, ctx, tx, atView(3))
		for _, want := range []string{`data-at="3"`, `data-t="32" data-k="water" aria-disabled="true"`, `data-t="0" data-k="grass" aria-disabled="true"`, "flooded I3", "position 3"} {
			if !strings.Contains(at, want) {
				t.Errorf("the town at 3 has no %s", want)
			}
		}
		if strings.Contains(at, `data-k="house"`) || strings.Contains(at, "town-later") || strings.Contains(at, "data-v=") {
			t.Errorf("the town at 3:\n%s", at)
		}
		later := render(t, ctx, tx, laterView(3, true))
		if strings.Count(later, `class="town-ev"`) != Later || !strings.Contains(later, `data-pos="4"`) || strings.Contains(later, `data-pos="7"`) {
			t.Errorf("the events after 3:\n%s", later)
		}
		page, err := renderPage(ctx, tx, ada)
		if err != nil {
			return err
		}
		var html strings.Builder
		page.Render(ctx, &html)
		for _, want := range []string{`<symbol id="town-house"`, `data-bind:_town.want`, fmt.Sprintf(`max="%d"`, last), `data-on:input__throttle.50ms.trailing`} {
			if !strings.Contains(html.String(), want) {
				t.Errorf("the page has no %s", want)
			}
		}
		return nil
	})
}

func render(t *testing.T, ctx context.Context, tx *sql.Tx, v live.View) string {
	t.Helper()
	f, err := v.Render(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	return string(f.([]byte))
}

// TestSeed lays the starting town out once.
func TestSeed(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, Seed(time.Now()))
	e.Do(t, Seed(time.Now()))
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		b, err := bounds(ctx, tx)
		if b != (Bounds{First: 1, Head: int64(len(start))}) {
			t.Errorf("after two seeds: %+v", b)
		}
		town, err := townNow(ctx, tx)
		if town[44].Kind != Bridge || town[25].Kind != House || town[2].Kind != Tree {
			t.Errorf("the starting town: %v", town)
		}
		return err
	})
}

func readTown(t *testing.T, e *demotest.Env, v demo.Viewer) (town Town, s TabState) {
	t.Helper()
	e.View(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		if town, err = townNow(ctx, tx); err != nil {
			return err
		}
		return demo.LoadTab(ctx, tx, Slug, v, &s)
	})
	return town, s
}
