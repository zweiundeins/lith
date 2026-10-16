package events_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/writer"
)

// An append and its projections commit together or not at all, in a
// batch of many commands with a savepoint each: a command that fails, in
// its projector, after Execute, by a panic, or in a nested savepoint,
// leaves no event, state row or projection row, nor its topics, and the
// commands around it commit.
func TestAllOrNothing(t *testing.T) {
	t.Run("warm", func(t *testing.T) { allOrNothing(t, true) })
	// The batch's first commands also create the tables, and the failed
	// ones take them back with their savepoint.
	t.Run("cold", func(t *testing.T) { allOrNothing(t, false) })
}

func allOrNothing(t *testing.T, warm bool) {
	f := open(t, titles(1, same))
	ctx := context.Background()
	if warm {
		f.create(t, 1000, "before")
		f.touched()
	}
	release, started := make(chan struct{}), make(chan struct{})
	f.w.Send(writer.Func(func(*writer.Tx) error { close(started); <-release; return nil }))
	<-started

	const n = 50
	errAfter := errors.New("refused after the append")
	queued := 0
	if !warm {
		// The batch's first command creates the tables and fails.
		go f.do(t, func(tx *writer.Tx) error {
			if err := events.Execute(f.st, tx, create, Cards.ID(999), events.NoStream, Create{Title: "x"}, events.Meta{}); err != nil {
				return err
			}
			return errAfter
		})
		queued = 1
		waitFor(t, "the first command queued", func() bool { return f.w.Stats().Queue == 1 })
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		id := int64(i + 1)
		exec := func(tx *writer.Tx, title string) error {
			return events.Execute(f.st, tx, create, Cards.ID(id), events.NoStream, Create{Title: title}, events.Meta{})
		}
		cmd := writer.Func(func(tx *writer.Tx) error {
			switch i % 5 {
			case 1:
				return exec(tx, "fail")
			case 2:
				if err := exec(tx, "kept?"); err != nil {
					return err
				}
				return errAfter
			case 3:
				if err := exec(tx, "kept?"); err != nil {
					return err
				}
				panic("after the append")
			case 4:
				err := tx.Savepoint(func(tx *writer.Tx) error {
					if err := exec(tx, "kept?"); err != nil {
						return err
					}
					return errAfter
				})
				if !errors.Is(err, errAfter) {
					return err
				}
				return nil
			}
			return exec(tx, "kept")
		})
		wg.Go(func() { errs[i] = f.w.Do(ctx, cmd) })
	}
	waitFor(t, "the commands queued", func() bool { return f.w.Stats().Queue == queued+n })
	close(release)
	wg.Wait()
	if b := f.w.Stats().MaxBatch; b < n {
		t.Fatalf("the commands ran in batches of up to %d, not one", b)
	}

	var kept []int64
	var wantTopics []string
	for i, err := range errs {
		id := int64(i + 1)
		var ok bool
		switch i % 5 {
		case 0, 4:
			ok = err == nil
		case 1:
			ok = errors.Is(err, errFail)
		case 2:
			ok = errors.Is(err, errAfter)
		case 3:
			ok = errors.Is(err, writer.ErrPanic)
		}
		if !ok {
			t.Errorf("command %d: %v", i, err)
		}
		if i%5 == 0 {
			kept = append(kept, id)
			wantTopics = append(wantTopics, cardTopic(id))
		}
	}
	if warm {
		kept = append(kept, 1000)
	}
	slices.Sort(kept)
	for _, q := range []string{
		`SELECT DISTINCT stream_id FROM lith_events ORDER BY 1`,
		`SELECT stream_id FROM lith_event_state ORDER BY 1`,
		`SELECT id FROM card_titles ORDER BY 1`,
	} {
		if got := f.ids(t, q); !slices.Equal(got, kept) {
			t.Errorf("%s: %v, want %v", q, got, kept)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM card_titles WHERE title != 'kept' AND id != 1000`); n != 0 {
		t.Errorf("%d titles of failed commands", n)
	}
	wantTopics = append(wantTopics, events.Topic)
	slices.Sort(wantTopics)
	if got := f.touched(); !slices.Equal(got, wantTopics) {
		t.Errorf("touched %v, want %v", got, wantTopics)
	}

	// The failures left the store's view of the database as it was.
	f.create(t, 2000, "after")
	if got := f.titles(t); got[2000] != "after" {
		t.Errorf("after the batch: %v", got)
	}
}

func (f *fixture) ids(t testing.TB, q string) []int64 {
	t.Helper()
	rows, err := f.db.R.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}
