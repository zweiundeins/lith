package guestbook

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demo/demotest"
)

var me = demo.Viewer{Session: "s1", Tab: "t1"}

func TestSign(t *testing.T) {
	e := demotest.Open(t, Demo)
	at := time.Unix(1_800_000_000, 0)
	sign := Sign{Viewer: me, Op: "op1", Name: " Ada ", Message: "Hello\x07 from Biel", At: at}
	e.Do(t, sign)
	list, total, state := read(t, e)
	if total != 1 || list[0].Name != "Ada" || list[0].Message != "Hello from Biel" || !state.Accepted() || state.Op != "op1" {
		t.Fatalf("signed: %d %+v, tab %+v", total, list, state)
	}
	if got := e.Touched(); !slices.Equal(got, []string{"guestbook", "tab:guestbook:t1"}) {
		t.Errorf("a signature touched %v", got)
	}

	// The newest entry again: refused, and only the tab hears why.
	sign.Op = "op2"
	e.Do(t, sign)
	_, total, state = read(t, e)
	if total != 1 || state.Op != "op2" || state.Accepted() || !strings.Contains(state.Refusal, "newest entry") {
		t.Fatalf("repeated: %d entries, tab %+v", total, state)
	}
	if got := e.Touched(); !slices.Equal(got, []string{"tab:guestbook:t1"}) {
		t.Errorf("a refusal touched %v", got)
	}

	// A link: refused.
	sign.Op, sign.Message = "op3", "cheap pills at www.example.com"
	e.Do(t, sign)
	if _, total, state = read(t, e); total != 1 || !strings.Contains(state.Refusal, "Links") {
		t.Fatalf("a link: %d entries, tab %+v", total, state)
	}

	// A blank name is the person's input: refused, and the tab hears why.
	sign.Op, sign.Name = "op4", "  "
	e.Do(t, sign)
	if _, total, state = read(t, e); total != 1 || state.Op != "op4" || !strings.Contains(state.Refusal, "A name has") {
		t.Fatalf("a blank name: %d entries, tab %+v", total, state)
	}

	// A signature without its op never comes from a page of ours.
	sign.Op = ""
	if err := e.W.Do(context.Background(), sign); !errors.Is(err, writer.ErrInvalid) {
		t.Errorf("no op: %v", err)
	}
}

func TestSweep(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, writer.Func(func(tx *writer.Tx) error {
		for range Kept + 5 {
			if _, err := tx.Exec(`INSERT INTO guestbook_entries (name, message, at) VALUES ('Ada', 'Hello', 0)`); err != nil {
				return err
			}
		}
		return nil
	}))
	e.Touched()
	e.Do(t, writer.Func(func(tx *writer.Tx) error { return sweep(tx, time.Now()) }))
	if _, total, _ := read(t, e); total != Kept {
		t.Errorf("after the sweep: %d entries", total)
	}
	if got := e.Touched(); !slices.Equal(got, []string{"guestbook"}) {
		t.Errorf("the sweep touched %v", got)
	}
	e.Do(t, writer.Func(func(tx *writer.Tx) error { return sweep(tx, time.Now()) }))
	if got := e.Touched(); len(got) != 0 {
		t.Errorf("a sweep that deleted nothing touched %v", got)
	}
}

// TestViews renders the views as a tab's stream gets them: the list as a
// frame whose later form animates, the tab's outcome with its op id.
func TestViews(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, Sign{Viewer: me, Op: "op1", Name: "Ada", Message: "Hello", At: time.Now()})
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		vs := pageViews(me)
		if len(vs) != 2 || vs[0].Key != "guestbook:entries" || vs[1].Key != "tab:guestbook:t1" {
			t.Fatalf("views: %v %v", vs[0].Key, vs[1].Key)
		}
		f, err := vs[0].Render(ctx, tx)
		if err != nil {
			return err
		}
		frame, ok := f.(demo.Frame)
		if !ok || strings.Contains(string(frame.First), "useViewTransition") || !strings.Contains(string(frame.Next), "useViewTransition true") {
			t.Errorf("the list's frame: %T", f)
		}
		if !strings.Contains(string(frame.First), "data-fresh") || !strings.Contains(string(frame.First), "view-transition-name: guestbook-entry-1") {
			t.Errorf("a new entry is not fresh:\n%s", frame.First)
		}
		out, err := vs[1].Render(ctx, tx)
		if err != nil {
			return err
		}
		b := string(out.([]byte))
		if !strings.Contains(b, "Thank you for signing.") || !strings.Contains(b, `"_guestbook":{"ack":"op1","accepted":true,"synced":true}`) {
			t.Errorf("the tab's view:\n%s", b)
		}
		page, err := renderPage(ctx, tx, me)
		if err != nil {
			return err
		}
		var html strings.Builder
		page.Render(ctx, &html)
		if !strings.Contains(html.String(), "Hello") || !strings.Contains(html.String(), `data-bind="_guestbook.name"`) {
			t.Errorf("the page:\n%s", html.String())
		}
		return nil
	})
}

func read(t *testing.T, e *demotest.Env) (list []Entry, total int, state TabState) {
	t.Helper()
	e.View(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		if list, total, err = latest(ctx, tx, Shown); err != nil {
			return err
		}
		return demo.LoadTab(ctx, tx, Slug, me, &state)
	})
	return list, total, state
}
