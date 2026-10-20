package writer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/zweiundeins/lith/sqlite"
)

// outcome is what a command left behind: its error, the items in the
// table, the records of Once and OnceResult, the topics OnCommit got and
// whether its AfterCommit ran.
type outcome struct {
	err     string
	items   []string
	ops     []string
	results []string
	topics  []string
	after   bool
	commits int // OnCommit calls
}

func (f *fixture) strings(t *testing.T, q string) []string {
	t.Helper()
	rows, err := f.db.R.Query(q)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil
		}
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// TestOneCommandBatches runs each kind of outcome as the only command of
// its batch, which runs without a savepoint, and after a neighbour in a
// batch of two, which runs in one: the outcomes must not differ.
func TestOneCommandBatches(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name string
		cmd  func(after *bool) Command
		want outcome
	}{
		{"success", func(after *bool) Command {
			return Func(func(tx *Tx) error {
				tx.Touch("t")
				tx.AfterCommit(func() { *after = true })
				return insert("x")(tx)
			})
		}, outcome{items: []string{"x"}, topics: []string{"t"}, after: true}},
		{"error", func(after *bool) Command {
			return Func(func(tx *Tx) error {
				tx.Touch("t")
				tx.AfterCommit(func() { *after = true })
				if err := insert("x")(tx); err != nil {
					return err
				}
				return boom
			})
		}, outcome{err: "boom"}},
		{"error with rows open", func(after *bool) Command {
			return Func(func(tx *Tx) error {
				if err := insert("x")(tx); err != nil {
					return err
				}
				if _, err := tx.Query("SELECT name FROM items"); err != nil {
					return err
				}
				return boom
			})
		}, outcome{err: "boom"}},
		{"panic", func(after *bool) Command {
			return Func(func(tx *Tx) error {
				tx.Touch("t")
				tx.AfterCommit(func() { *after = true })
				if err := insert("x")(tx); err != nil {
					return err
				}
				panic("broken")
			})
		}, outcome{err: "writer: command panicked: broken"}},
		{"failed statement", func(after *bool) Command {
			return Func(func(tx *Tx) error {
				if err := insert("x")(tx); err != nil {
					return err
				}
				return insert("x")(tx) // UNIQUE
			})
		}, outcome{err: "constraint failed: UNIQUE constraint failed: items.name (2067)"}},
		{"inner savepoint fails", func(after *bool) Command {
			return Func(func(tx *Tx) error {
				if err := insert("x")(tx); err != nil {
					return err
				}
				tx.Savepoint(func(tx *Tx) error {
					tx.Touch("inner")
					insert("y")(tx)
					return boom
				})
				tx.Touch("t")
				return nil
			})
		}, outcome{items: []string{"x"}, topics: []string{"t"}}},
		{"Once", func(after *bool) Command {
			return Once("op", Func(func(tx *Tx) error {
				tx.Touch("t")
				return insert("x")(tx)
			}))
		}, outcome{items: []string{"x"}, ops: []string{"op"}, topics: []string{"t"}}},
		{"Once refused", func(after *bool) Command {
			return Once("op", Func(func(tx *Tx) error {
				if err := insert("x")(tx); err != nil {
					return err
				}
				return boom
			}))
		}, outcome{err: "boom"}},
		{"OnceResult", func(after *bool) Command {
			return OnceResult("op", ResultFunc(func(tx *Tx) ([]byte, error) {
				tx.Touch("t")
				return []byte("42"), insert("x")(tx)
			}))
		}, outcome{items: []string{"x"}, results: []string{"op 42 -"}, topics: []string{"t"}}},
		{"OnceResult refused", func(after *bool) Command {
			return OnceResult("op", ResultFunc(func(tx *Tx) ([]byte, error) {
				tx.Touch("t")
				tx.AfterCommit(func() { *after = true })
				if err := insert("x")(tx); err != nil {
					return nil, err
				}
				return []byte("no"), &Refusal{Msg: "full"}
			}))
		}, outcome{err: "full", results: []string{"op no full"}}},
		{"OnceResult failed", func(after *bool) Command {
			return OnceResult("op", ResultFunc(func(tx *Tx) ([]byte, error) {
				if err := insert("x")(tx); err != nil {
					return nil, err
				}
				return nil, boom
			}))
		}, outcome{err: "boom"}},
	}
	run := func(t *testing.T, cmd func(*bool) Command, batched bool) outcome {
		var mu sync.Mutex
		var got outcome
		f := start(t, sqlite.Options{}, Options{
			Log: slog.New(slog.DiscardHandler),
			OnCommit: func(topics []string) {
				mu.Lock()
				got.commits++
				got.topics = append(got.topics, topics...)
				mu.Unlock()
			},
		})
		ctx := context.Background()
		var after bool
		var err error
		if batched {
			release := f.block(t)
			var wg sync.WaitGroup
			wg.Go(func() { f.w.Do(ctx, Func(func(tx *Tx) error { return nil })) })
			f.queued(t, 1)
			wg.Go(func() { err = f.w.Do(ctx, cmd(&after)) })
			f.queued(t, 2)
			release()
			wg.Wait()
			if s := f.w.Stats(); s.MaxBatch != 2 || s.Sizes[1] != 1 {
				t.Fatalf("not a batch of two: %+v", s)
			}
		} else {
			s0 := f.w.Stats()
			err = f.w.Do(ctx, cmd(&after))
			if s := f.w.Stats(); s.Batches != s0.Batches+1 || s.Sizes[0] != s0.Sizes[0]+1 {
				t.Fatalf("not a batch of one: %+v", s)
			}
		}
		if err != nil {
			got.err = err.Error()
		}
		got.after = after
		got.items = f.strings(t, "SELECT name FROM items ORDER BY name")
		got.ops = f.strings(t, "SELECT key FROM lith_ops")
		got.results = f.strings(t, "SELECT key || ' ' || coalesce(CAST(result AS TEXT), '-') || ' ' || coalesce(refusal, '-') FROM lith_results")
		mu.Lock()
		// Batched, the block commits too.
		if want := map[bool]int{false: 1, true: 2}[batched]; got.commits != want {
			t.Errorf("OnCommit ran %d times, want %d", got.commits, want)
		}
		mu.Unlock()

		// The writer goes on.
		if err := f.w.Do(ctx, insert("next")); err != nil {
			t.Fatal(err)
		}
		if names := f.strings(t, "SELECT name FROM items WHERE name = 'next'"); len(names) != 1 {
			t.Fatal("the next command did not commit")
		}
		mu.Lock()
		defer mu.Unlock()
		got.commits = 0
		return got
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			alone := run(t, c.cmd, false)
			batched := run(t, c.cmd, true)
			if fmt.Sprint(alone) != fmt.Sprint(c.want) {
				t.Errorf("alone:   %+v\nwant     %+v", alone, c.want)
			}
			if fmt.Sprint(batched) != fmt.Sprint(alone) {
				t.Errorf("batched: %+v\nalone:   %+v", batched, alone)
			}
		})
	}
}

// TestOneCommandBatchesReplay replays recorded answers in batches of one.
func TestOneCommandBatchesReplay(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{})
	ctx := context.Background()
	refuse := func() *Op {
		return OnceResult("r", ResultFunc(func(tx *Tx) ([]byte, error) {
			insert("x")(tx)
			return []byte("no"), &Refusal{Msg: "full"}
		}))
	}
	for i := range 2 {
		op := refuse()
		var refusal *Refusal
		if err := f.w.Do(ctx, op); !errors.As(err, &refusal) || refusal.Msg != "full" || string(op.Result()) != "no" || op.Replayed() != (i == 1) {
			t.Fatalf("run %d: %v, %q, replayed %v", i, err, op.Result(), op.Replayed())
		}
	}
	if items := f.strings(t, "SELECT name FROM items"); len(items) != 0 {
		t.Fatalf("a refusal kept its changes: %v", items)
	}
	if s := f.w.Stats(); s.Sizes[0] != s.Batches || s.Failures != 2 {
		t.Fatalf("%+v", s)
	}
}

// TestDoAllocations counts what Do and the writer allocate for a command
// besides the statement it runs.
func TestDoAllocations(t *testing.T) {
	f := start(t, sqlite.Options{}, Options{OnCommit: func([]string) {}})
	ctx := context.Background()
	touch := Func(func(tx *Tx) error { tx.Touch("t"); return nil })
	for range 10 {
		f.w.Do(ctx, touch)
	}
	// BEGIN and COMMIT allocate in database/sql and the driver, the
	// writer the Tx and OnCommit's topics; Do and Touch nothing. A
	// command alone in its batch runs no SAVEPOINT and RELEASE. v0.4.0
	// took 19.
	if n := testing.AllocsPerRun(200, func() { f.w.Do(ctx, touch) }); n > 8 {
		t.Errorf("%.1f allocations a command", n)
	}
}
