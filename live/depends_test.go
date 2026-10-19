package live

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// routeDB is testDB with a second counter, b, and a tab whose route names
// the counter it shows.
func routeDB(t *testing.T) (write, read *sql.DB) {
	t.Helper()
	write, read = testDB(t, 4)
	exec(t, write, `insert into counter values ('b', 0); create table tab (route text not null); insert into tab values ('a')`)
	return write, read
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

// routeView renders the counter the tab's route names. The tab's row is a
// static topic, and the counter's topic follows the route. after, if set,
// runs once the Render has read its snapshot.
func routeView(after func(route string)) View {
	return text("tab", []string{"tab"}, func(ctx context.Context, tx *sql.Tx) (string, error) {
		var route string
		if err := tx.QueryRowContext(ctx, `select route from tab`).Scan(&route); err != nil {
			return "", err
		}
		Depends(ctx, "counter:"+route)
		var n int
		if err := tx.QueryRowContext(ctx, `select n from counter where name = ?`, route).Scan(&n); err != nil {
			return "", err
		}
		if after != nil {
			after(route)
		}
		return route + "=" + strconv.Itoa(n), nil
	})
}

// A tab that navigates writes its row; its view declares the topics of the
// route it rendered, so a commit to the new route renders it and one to
// the old route does not, without Unwatch and Watch.
func TestDependsFollowsNavigation(t *testing.T) {
	write, read := routeDB(t)
	h := start(t, Options{Read: read, Tick: 5 * time.Millisecond})
	commit := func(query string, topic string) {
		exec(t, write, query)
		h.Changed([]string{topic})
	}
	c := serve(h, Spec{ID: "t1", Views: []View{routeView(nil)}}, "")
	waitFor(t, "the first frame", func() bool { return c.has(`<p id="tab">a=0</p>`) })
	commit(`update counter set n = n + 1 where name = 'a'`, "counter:a")
	waitFor(t, "a's change", func() bool { return c.has(`<p id="tab">a=1</p>`) })

	commit(`update tab set route = 'b'`, "tab")
	waitFor(t, "the new route", func() bool { return c.has(`<p id="tab">b=0</p>`) })
	renders := h.Stats().RendersByKey["tab"]
	commit(`update counter set n = n + 1 where name = 'a'`, "counter:a")
	time.Sleep(50 * time.Millisecond)
	if n := h.Stats().RendersByKey["tab"]; n != renders {
		t.Fatalf("a commit to the old route rendered the view %d times", n-renders)
	}
	commit(`update counter set n = n + 1 where name = 'b'`, "counter:b")
	waitFor(t, "b's change", func() bool { return c.has(`<p id="tab">b=1</p>`) })

	c.leave(t)
	h.mu.Lock()
	n := len(h.topics)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d topics left after the stream left", n)
	}
}

// A commit that lands after a Render's snapshot, to a topic the Render
// declares for the first time, comes before the hub takes the topic on:
// no Changed finds the view for it. The hub sees from the topic's version
// that the snapshot may lack the commit, and renders the view again.
func TestDependsSeesACommitBetweenSnapshotAndSubscription(t *testing.T) {
	write, read := routeDB(t)
	h := start(t, Options{Read: read, Tick: 5 * time.Millisecond})
	var once atomic.Bool
	v := routeView(func(route string) {
		if route == "b" && once.CompareAndSwap(false, true) {
			exec(t, write, `update counter set n = n + 1 where name = 'b'`)
			h.Changed([]string{"counter:b"})
		}
	})
	c := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the first frame", func() bool { return c.has(`<p id="tab">a=0</p>`) })
	exec(t, write, `update tab set route = 'b'`)
	h.Changed([]string{"tab"})
	waitFor(t, "the render that missed the commit", func() bool { return c.has(`<p id="tab">b=0</p>`) })
	waitFor(t, "the commit it missed", func() bool { return c.has(`<p id="tab">b=1</p>`) })
}

// A Render that fails keeps the topics the one before declared, and a
// view that declares none behaves as one with only Topics.
func TestDependsKeptWhenRenderFails(t *testing.T) {
	h := start(t, Options{Tick: 5 * time.Millisecond})
	Depends(context.Background(), "x") // outside a Render: nothing
	var calls atomic.Int64
	v := text("v", nil, func(ctx context.Context, _ *sql.Tx) (string, error) {
		n := calls.Add(1)
		if n == 2 {
			return "", errors.New("boom")
		}
		Depends(ctx, "x", "x")
		return strconv.Itoa(int(n)), nil
	})
	c := serve(h, Spec{Views: []View{v}}, "")
	waitFor(t, "the first frame", func() bool { return c.has(`<p id="v">1</p>`) })
	h.Changed([]string{"x"})
	waitFor(t, "the failed render", func() bool { return calls.Load() == 2 })
	h.Changed([]string{"x"})
	waitFor(t, "a render after the failure", func() bool { return c.has(`<p id="v">3</p>`) })
}
