package events_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/writer"
)

// Random histories of commands, some failing, some in one batch, some
// several to a command, keep what the store promises: each stream's
// versions run from 1 without a gap, its state row is the fold of its
// events, card_titles is what the handlers make of the events, and a
// rebuild makes the same table.
func TestRandomHistories(t *testing.T) {
	for seed := range 6 {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { randomHistory(t, uint64(seed)) })
	}
}

func randomHistory(t *testing.T, seed uint64) {
	rnd := rand.New(rand.NewPCG(seed, 7))
	f := open(t, titles(1, same))
	const cards = 12
	errRefused := errors.New("refused")
	exec := func(tx *writer.Tx) error {
		id := Cards.ID(1 + rnd.Int64N(cards))
		var err error
		switch rnd.IntN(7) {
		case 0, 1:
			err = events.Execute(f.st, tx, create, id, events.Any, Create{Title: fmt.Sprint("t", rnd.IntN(4))}, events.Meta{})
		case 2, 3:
			err = events.Execute(f.st, tx, rename, id, events.Any, Rename{Title: fmt.Sprint("t", rnd.IntN(4))}, events.Meta{})
		case 4:
			err = events.Execute(f.st, tx, archive, id, events.Any, Archive{}, events.Meta{})
		case 5:
			err = events.Append(f.st, tx, id, events.Any, events.Meta{Actor: "import"}, events.One[CardEvent](CardRenamed{To: fmt.Sprint("i", rnd.IntN(4))}))
		case 6:
			if err = events.Execute(f.st, tx, create, id, events.Any, Create{Title: "x"}, events.Meta{}); err == nil {
				err = errRefused
			}
		}
		return err
	}
	ctx := context.Background()
	for range 150 {
		switch rnd.IntN(3) {
		case 0:
			f.w.Do(ctx, writer.Func(exec))
		case 1:
			n := 1 + rnd.IntN(4)
			f.w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
				for range n {
					if err := tx.Savepoint(exec); err != nil && rnd.IntN(3) == 0 {
						return err
					}
				}
				return nil
			}))
		case 2:
			for range 1 + rnd.IntN(8) {
				f.w.Send(writer.Func(exec))
			}
			f.w.Flush(ctx)
		}
	}

	want := map[int64]card{}
	for id := int64(1); id <= cards; id++ {
		recs := f.load(t, id)
		var s card
		var last int64
		for i, r := range recs {
			if r.Version != int64(i+1) || r.Position <= last {
				t.Fatalf("card %d: event %d at version %d, position %d after %d", id, i, r.Version, r.Position, last)
			}
			last = r.Position
			s = evolveCard(s, r.Data)
		}
		if len(recs) == 0 {
			continue
		}
		var data string
		err := f.db.R.QueryRow(`SELECT state FROM lith_event_state WHERE stream = 'card' AND stream_id = ? AND version = ?`, id, len(recs)).Scan(&data)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// appended last: the next Execute folds it
		case err != nil:
			t.Fatal(err)
		default:
			var stored card
			if err := json.Unmarshal([]byte(data), &stored); err != nil || stored != s {
				t.Fatalf("card %d: state %s, folded %+v", id, data, s)
			}
		}
		// The titles projector inserts at creation and updates after: an
		// import's rename of a card not created leaves no row.
		if slices.ContainsFunc(recs, func(r events.Record[int64, CardEvent]) bool { return r.Type == "card.created" }) {
			want[id] = s
		}
	}
	got := f.titles(t)
	if len(got) != len(want) {
		t.Fatalf("card_titles has %d cards, the events %d", len(got), len(want))
	}
	for id, s := range want {
		if got[id] != s.Title {
			t.Fatalf("card %d: %q in card_titles, %q in its events", id, got[id], s.Title)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM card_titles WHERE archived = 1`); n != archivedCount(want) {
		t.Fatalf("%d archived in card_titles, %d in the events", n, archivedCount(want))
	}

	v2 := f.reopen(titles(2, same))
	r := v2.run(t, events.Options{Batch: 1 + rnd.IntN(20)})
	waitFor(t, "the rebuild", func() bool { return status(r, "card_titles").Status == "live" })
	if after := v2.titles(t); fmt.Sprint(after) != fmt.Sprint(got) {
		t.Fatalf("the rebuild made %v of %v", after, got)
	}
}

func archivedCount(m map[int64]card) int {
	n := 0
	for _, s := range m {
		if s.Archived {
			n++
		}
	}
	return n
}
