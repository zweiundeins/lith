package writer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// OnceResult records a command's answer, its refusals included: a retry
// gets the first answer, a refusal with its message, without running the
// command again, while a failure leaves no record.
func TestOnceResult(t *testing.T) {
	var mu sync.Mutex
	var published [][]string
	f := start(t, sqlite.Options{}, Options{OnCommit: func(ts []string) {
		mu.Lock()
		published = append(published, ts)
		mu.Unlock()
	}})
	ctx := context.Background()
	runs := 0
	add := func(name string) ResultCommand {
		return ResultFunc(func(tx *Tx) ([]byte, error) {
			runs++
			res, err := tx.Exec("INSERT INTO items (name) VALUES (?)", name)
			if err != nil {
				return nil, err
			}
			id, err := res.LastInsertId()
			tx.Touch("items")
			return []byte(fmt.Sprint(id)), err
		})
	}
	refuse := ResultFunc(func(tx *Tx) ([]byte, error) {
		runs++
		if err := insert("refused")(tx); err != nil {
			return nil, err
		}
		tx.Touch("refused")
		return []byte("version 7"), fmt.Errorf("move: %w", &Refusal{Msg: "the lane is full"})
	})

	op := OnceResult("op1", add("a"))
	if err := f.w.Do(ctx, op); err != nil || string(op.Result()) != "1" || op.Replayed() {
		t.Fatalf("first run: %v, result %q, replayed %v", err, op.Result(), op.Replayed())
	}
	again := OnceResult("op1", add("a"))
	if err := f.w.Do(ctx, again); err != nil || string(again.Result()) != "1" || !again.Replayed() || runs != 1 {
		t.Fatalf("retry: %v, result %q, replayed %v, %d runs", err, again.Result(), again.Replayed(), runs)
	}

	for i := range 2 {
		op := OnceResult("op2", refuse)
		err := f.w.Do(ctx, op)
		var r *Refusal
		if !errors.As(err, &r) || r.Msg != "the lane is full" || string(op.Result()) != "version 7" || op.Replayed() != (i == 1) {
			t.Fatalf("refusal, run %d: %v, result %q, replayed %v", i, err, op.Result(), op.Replayed())
		}
	}
	if runs != 2 {
		t.Fatalf("the refused command ran %d times", runs-1)
	}
	if n := f.count(t, "SELECT count(*) FROM items WHERE name = 'refused'"); n != 0 {
		t.Fatal("a refusal kept its changes")
	}

	broken := ResultFunc(func(tx *Tx) ([]byte, error) { runs++; return nil, errors.New("disk on fire") })
	for range 2 {
		if err := f.w.Do(ctx, OnceResult("op3", broken)); err == nil || strings.Contains(err.Error(), "lane") {
			t.Fatalf("a failure: %v", err)
		}
	}
	if runs != 4 {
		t.Fatal("a failed command was recorded")
	}

	mu.Lock()
	defer mu.Unlock()
	if want := [][]string{{"items"}, nil, nil, nil, nil, nil}; !slices.EqualFunc(published, want, slices.Equal) {
		t.Fatalf("published %q, want %q", published, want)
	}
	for _, cmd := range []Command{OnceResult("", add("x")), OnceResult("op4", nil)} {
		if err := f.w.Do(ctx, cmd); !errors.Is(err, ErrInvalid) {
			t.Fatalf("got %v", err)
		}
	}
}

// A refusal recorded inside a command's savepoint stays, and the command
// goes on.
func TestOnceResultInASavepoint(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	refuse := ResultFunc(func(tx *Tx) ([]byte, error) { return nil, &Refusal{Msg: "no"} })
	err := f.w.Do(context.Background(), Func(func(tx *Tx) error {
		var r *Refusal
		if err := tx.Savepoint(OnceResult("op", refuse).Apply); !errors.As(err, &r) {
			return fmt.Errorf("the savepoint returned %v", err)
		}
		return insert("after")(tx)
	}))
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT count(*) FROM lith_results WHERE key = 'op' AND refusal = 'no'"); n != 1 {
		t.Fatal("the refusal was not recorded")
	}
	if n := f.count(t, "SELECT count(*) FROM items WHERE name = 'after'"); n != 1 {
		t.Fatal("the command did not go on")
	}
}

func TestPruneOnceForgetsResults(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := context.Background()
	ok := ResultFunc(func(*Tx) ([]byte, error) { return []byte("x"), nil })
	for _, key := range []string{"old", "new"} {
		if err := f.w.Do(ctx, OnceResult(key, ok)); err != nil {
			t.Fatal(err)
		}
	}
	err := f.w.Do(ctx, Func(func(tx *Tx) error {
		_, err := tx.Exec("UPDATE lith_results SET at = at - 7200 WHERE key = 'old'")
		return err
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.w.Do(ctx, PruneOnce(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "SELECT count(*) FROM lith_results"); n != 1 {
		t.Fatalf("%d results left, want the new one", n)
	}
}
