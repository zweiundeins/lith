package events_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/writer"
)

func (f *fixture) maxPosition(t testing.TB) int64 {
	t.Helper()
	var n int64
	if err := f.db.R.QueryRow(`SELECT coalesce(max(position), 0) FROM lith_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Positions are never handed out twice, not after Forget took the newest
// stream nor after Prune took every event of a kind: a catch-up
// projector that remembers a position never skips the event that would
// otherwise come under it.
func TestPositionsNeverReused(t *testing.T) {
	f := open(t, titles(1, same))
	f.createMany(t, 1, 3)
	f.rename(t, 3, "renamed")
	last := f.maxPosition(t)
	if err := f.do(t, func(tx *writer.Tx) error {
		n, err := events.Forget(f.st, tx, Cards.ID(3))
		if err != nil || n != 2 {
			t.Errorf("forgot %d events, %v", n, err)
		}
		_, err = tx.Exec(`DELETE FROM card_titles WHERE id = 3`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM lith_event_state WHERE stream_id = 3`); n != 0 {
		t.Error("the forgotten card's state is left")
	}
	f.create(t, 4, "d")
	if p := f.position(t, 4); p <= last {
		t.Errorf("position %d after %d", p, last)
	}
	last = f.maxPosition(t)
	if err := f.do(t, func(tx *writer.Tx) error {
		_, err := events.Prune(f.st, tx, Cards, events.Retention{Since: time.Now().Add(time.Hour)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM lith_events`); n != 0 {
		t.Fatalf("%d events after Prune", n)
	}
	f.create(t, 5, "e")
	if p := f.position(t, 5); p <= last {
		t.Errorf("position %d after %d", p, last)
	}
	f.create(t, 3, "again") // a forgotten stream starts at version 1
}

// Prune takes a kind's oldest events, keeping the state rows and the
// projections, and none that a catch-up projector has not passed.
func TestPrune(t *testing.T) {
	m := newMailer()
	f := open(t, titles(1, same), m.projector(1))
	old := time.Now().Add(-48 * time.Hour)
	for id := int64(1); id <= 5; id++ {
		if err := f.do(t, func(tx *writer.Tx) error {
			return events.Execute(f.st, tx, create, Cards.ID(id), events.NoStream, Create{Title: "old"}, events.Meta{At: old})
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.create(t, 6, "new")
	if err := f.do(t, func(tx *writer.Tx) error {
		return events.Append(f.st, tx, Notes.ID(1), events.Any, events.Meta{At: old}, events.One[NoteEvent](NoteAdded{Body: "old"}))
	}); err != nil {
		t.Fatal(err)
	}
	prune := func(limit int) int64 {
		t.Helper()
		var n int64
		if err := f.do(t, func(tx *writer.Tx) (err error) {
			n, err = events.Prune(f.st, tx, Cards, events.Retention{Since: time.Now().Add(-24 * time.Hour), Limit: limit})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := prune(0); n != 0 {
		t.Errorf("pruned %d events the mailer has not seen", n)
	}
	f.run(t, events.Options{})
	waitFor(t, "the mails", func() bool {
		return f.count(t, `SELECT count(*) FROM lith_projections WHERE name = 'mailer' AND position = ?`, f.position(t, 6)) == 1
	})
	for _, c := range []struct {
		limit int
		want  int64
	}{{2, 2}, {0, 3}, {0, 0}} {
		if n := prune(c.limit); n != c.want {
			t.Errorf("pruned %d, want %d", n, c.want)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM lith_events`); n != 2 {
		t.Errorf("%d events left, want card 6's and the note's", n)
	}
	if n := f.count(t, `SELECT count(*) FROM lith_event_state`); n != 6 {
		t.Errorf("%d state rows", n)
	}
	if n := len(f.titles(t)); n != 6 {
		t.Errorf("%d titles", n)
	}
	if err := f.do(t, func(tx *writer.Tx) error {
		return events.Execute(f.st, tx, rename, Cards.ID(1), events.Exactly(1), Rename{Title: "renamed"}, events.Meta{})
	}); err != nil {
		t.Fatal(err)
	}
	if recs := f.load(t, 1); len(recs) != 1 || recs[0].Version != 2 {
		t.Errorf("card 1's events %+v", recs)
	}
}

// Prune keeps events by time, by count, or both: an event goes only when
// every rule set lets it go. Limit spreads a backlog over calls, and a
// Retention without a rule is refused.
func TestRetention(t *testing.T) {
	f := open(t, titles(1, same))
	old := time.Now().Add(-48 * time.Hour)
	for id := int64(1); id <= 10; id++ {
		if err := f.do(t, func(tx *writer.Tx) error {
			return events.Execute(f.st, tx, create, Cards.ID(id), events.NoStream, Create{Title: "old"}, events.Meta{At: old})
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.createMany(t, 11, 20)
	day := time.Now().Add(-24 * time.Hour)
	prune := func(keep events.Retention) int64 {
		t.Helper()
		var n int64
		if err := f.do(t, func(tx *writer.Tx) (err error) {
			n, err = events.Prune(f.st, tx, Cards, keep)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	bounds := func() (first, last int64) {
		t.Helper()
		err := f.db.View(context.Background(), func(tx *sql.Tx) (err error) {
			first, last, err = events.Bounds(context.Background(), tx, Cards)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return first, last
	}
	if first, last := bounds(); first != 1 || last != 20 {
		t.Errorf("bounds %d to %d", first, last)
	}
	if err := f.do(t, func(tx *writer.Tx) error {
		_, err := events.Prune(f.st, tx, Cards, events.Retention{Limit: 5})
		return err
	}); err == nil {
		t.Error("a Retention without a rule pruned")
	}
	for _, c := range []struct {
		keep        events.Retention
		want, first int64
	}{
		{events.Retention{Newest: 100}, 0, 1},
		{events.Retention{Since: day, Newest: 15}, 5, 6}, // the new ten and the newest five old ones
		{events.Retention{Newest: 12, Limit: 2}, 2, 8},
		{events.Retention{Newest: 12, Limit: 2}, 1, 9},
		{events.Retention{Newest: 12, Limit: 2}, 0, 9},
		{events.Retention{Since: day}, 2, 11},
		{events.Retention{Since: time.Now().Add(time.Hour), Newest: 3}, 7, 18},
	} {
		if n := prune(c.keep); n != c.want {
			t.Errorf("%+v pruned %d, want %d", c.keep, n, c.want)
		}
		if first, last := bounds(); first != c.first || last != 20 {
			t.Errorf("%+v: bounds %d to %d, want %d to 20", c.keep, first, last, c.first)
		}
	}
	if n := len(f.titles(t)); n != 20 {
		t.Errorf("%d titles", n)
	}
}
