package events_test

import (
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/writer"
)

// The store counts the events that commit by stream kind, the conflicts
// and the refolds; a command that fails counts no events.
func TestStoreStats(t *testing.T) {
	f := open(t, titles(1, same))
	if got := f.st.Stats(); !maps.Equal(got.Appended, map[string]int64{"card": 0}) || got.Conflicts != 0 || got.Refolds != 0 {
		t.Errorf("a new store: %+v", got)
	}
	f.create(t, 1, "a")
	f.rename(t, 1, "b")
	if err := f.do(t, func(tx *writer.Tx) error {
		return events.Execute(f.st, tx, create, Cards.ID(2), events.NoStream, Create{Title: "fail"}, events.Meta{})
	}); !errors.Is(err, errFail) {
		t.Fatalf("a failing projector: %v", err)
	}
	if err := f.do(t, func(tx *writer.Tx) error {
		return events.Execute(f.st, tx, rename, Cards.ID(1), events.Exactly(1), Rename{Title: "c"}, events.Meta{})
	}); !errors.Is(err, events.ErrConflict) {
		t.Fatalf("Execute at a stale version: %v", err)
	}
	if err := f.do(t, func(tx *writer.Tx) error {
		return events.Append(f.st, tx, Cards.ID(1), events.Exactly(1), events.Meta{}, events.One[CardEvent](CardArchived{}))
	}); !errors.Is(err, events.ErrConflict) {
		t.Fatalf("Append at a stale version: %v", err)
	}
	if err := f.do(t, func(tx *writer.Tx) error {
		return events.Append(f.st, tx, Cards.ID(1), events.Exactly(2), events.Meta{}, events.One[CardEvent](CardRenamed{From: "b", To: "c"}))
	}); err != nil {
		t.Fatal(err)
	}
	f.rename(t, 1, "d") // folds the stream again: Append dropped its state row
	f.rename(t, 1, "e")
	if err := f.do(t, func(tx *writer.Tx) error {
		return events.Append(f.st, tx, Users.ID("ada"), events.NoStream, events.Meta{}, events.One[UserEvent](UserJoined{Name: "Ada"}))
	}); err != nil {
		t.Fatal(err)
	}
	want := events.StoreStats{Appended: map[string]int64{"card": 5, "user": 1}, Conflicts: 2, Refolds: 1}
	if got := f.st.Stats(); !maps.Equal(got.Appended, want.Appended) || got.Conflicts != want.Conflicts || got.Refolds != want.Refolds {
		t.Errorf("stats %+v, want %+v", got, want)
	}
}

// The runner keeps the last position committed, and the lag of each
// catch-up projector behind the last event of its stream kind: events of
// other kinds move the head and not the lag.
func TestHeadAndLag(t *testing.T) {
	m := newMailer()
	f := open(t, titles(1, same), m.projector(1))
	f.create(t, 1, "a")
	r := f.run(t, events.Options{Retries: 1, Backoff: time.Millisecond})
	first := f.position(t, 1)
	waitFor(t, "the head read at the start", func() bool { return r.Stats().Head == first })
	waitFor(t, "the first mail", func() bool { return status(r, "mailer").Position == first })
	if s := status(r, "mailer"); s.Lag != 0 {
		t.Errorf("a projector that caught up: %+v", s)
	}

	m.set("poison", -1)
	f.create(t, 2, "poison")
	waitFor(t, "the projector parked", func() bool { return status(r, "mailer").Status == "parked" })
	f.create(t, 3, "c")
	last := f.position(t, 3)
	if s := r.Stats(); s.Head != last {
		t.Errorf("head %d, want %d", s.Head, last)
	}
	if s := status(r, "mailer"); s.Lag != last-first || s.Position != first {
		t.Errorf("a parked projector: %+v, want a lag of %d", s, last-first)
	}
	if s := status(r, "card_titles"); s.Lag != 0 {
		t.Errorf("an inline projector: %+v", s)
	}

	if err := f.do(t, func(tx *writer.Tx) error {
		return events.Append(f.st, tx, Users.ID("ada"), events.NoStream, events.Meta{}, events.One[UserEvent](UserJoined{Name: "Ada"}))
	}); err != nil {
		t.Fatal(err)
	}
	if s := r.Stats(); s.Head != last+1 {
		t.Errorf("head %d after another kind's event, want %d", s.Head, last+1)
	}
	if s := status(r, "mailer"); s.Lag != last-first {
		t.Errorf("another kind's event moved the lag: %+v", s)
	}

	m.set("poison", 0)
	if err := r.Resume("mailer"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the lag after Resume", func() bool {
		s := status(r, "mailer")
		return s.Status == "live" && s.Position == last && s.Lag == 0
	})
}
