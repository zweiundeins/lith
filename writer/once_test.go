package writer

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sqlite"
)

func TestOnce(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := context.Background()
	table := func() bool {
		return f.count(t, "SELECT count(*) FROM sqlite_master WHERE name = 'lith_ops'") == 1
	}
	if table() {
		t.Fatal("lith_ops exists before any Once")
	}

	// A failed command leaves no record, not even the table.
	refused := errors.New("refused")
	err := f.w.Do(ctx, Once("op1", Func(func(tx *Tx) error {
		if err := insert("x")(tx); err != nil {
			return err
		}
		return refused
	})))
	if !errors.Is(err, refused) {
		t.Fatalf("got %v", err)
	}
	if table() {
		t.Fatal("a failed command created lith_ops")
	}

	// The retry runs, and a second retry gets the first answer without running.
	if err := f.w.Do(ctx, Once("op1", insert("x"))); err != nil {
		t.Fatal(err)
	}
	ran := false
	err = f.w.Do(ctx, Once("op1", Func(func(tx *Tx) error {
		ran = true
		return insert("x")(tx)
	})))
	if err != nil || ran {
		t.Fatalf("a recorded key ran again: %v, ran %v", err, ran)
	}
	if n := f.count(t, "SELECT count(*) FROM items WHERE name = 'x'"); n != 1 {
		t.Fatalf("%d rows", n)
	}

	// The same key twice in one batch.
	release := f.block(t)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() {
			if err := f.w.Do(ctx, Once("op2", increment(1))); err != nil {
				t.Error(err)
			}
		})
		f.queued(t, i+1)
	}
	release()
	wg.Wait()
	if n := f.count(t, "SELECT n FROM counters WHERE id = 1"); n != 1 {
		t.Fatalf("counter %d, want 1", n)
	}

	for _, cmd := range []Command{Once("", increment(1)), Once("op3", nil), Once("op3", checked{})} {
		if err := f.w.Do(ctx, cmd); !errors.Is(err, ErrInvalid) {
			t.Fatalf("got %v", err)
		}
	}
}

func TestOnceKeepsTopicsAndAfterCommit(t *testing.T) {
	var published [][]string
	f := start(t, sqlite.Options{}, Options{OnCommit: func(ts []string) { published = append(published, ts) }})
	after := 0
	cmd := Func(func(tx *Tx) error {
		tx.Touch("board:1")
		tx.AfterCommit(func() { after++ })
		return nil
	})
	for range 2 {
		if err := f.w.Do(context.Background(), Once("op", cmd)); err != nil {
			t.Fatal(err)
		}
	}
	// The replay touches nothing and registers nothing.
	if len(published) != 2 || !slices.Equal(published[0], []string{"board:1"}) || published[1] != nil || after != 1 {
		t.Fatalf("published %q, AfterCommit ran %d times", published, after)
	}
}

func TestPruneOnce(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := context.Background()
	if err := f.w.Do(ctx, PruneOnce(time.Hour)); err != nil {
		t.Fatalf("prune before any Once: %v", err)
	}
	for _, key := range []string{"old", "new"} {
		if err := f.w.Do(ctx, Once(key, increment(1))); err != nil {
			t.Fatal(err)
		}
	}
	err := f.w.Do(ctx, Func(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE lith_ops SET at = at - 7200 WHERE key = 'old'")
		return err
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.w.Do(ctx, PruneOnce(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT count(*) FROM lith_ops WHERE key = 'new'"); n != 1 {
		t.Fatal("pruned a recent key")
	}
	if n := f.count(t, "SELECT count(*) FROM lith_ops WHERE key = 'old'"); n != 0 {
		t.Fatal("kept an old key")
	}
	if err := f.w.Do(ctx, Once("old", increment(1))); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT n FROM counters WHERE id = 1"); n != 3 {
		t.Fatalf("counter %d: a forgotten key should run again", n)
	}
}
