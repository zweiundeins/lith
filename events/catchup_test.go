package events_test

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zweiundeins/lith/events"
)

// mailer is a catch-up projector that mails a card's creation and keeps
// the positions it mailed, its idempotency keys.
type mailer struct {
	mu   sync.Mutex
	sent []int64
	fail map[string]int // failures left by title, -1 for ever
}

func newMailer() *mailer { return &mailer{fail: map[string]int{}} }

func (m *mailer) projector(version int) *events.Projector[int64, CardEvent] {
	return events.NewProjector(Cards, "mailer", version,
		events.After(Created, func(ctx context.Context, r events.Record[int64, CardCreated]) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			switch n := m.fail[r.Data.Title]; {
			case n == -2:
				m.fail[r.Data.Title] = 0
				panic("the mailer broke")
			case n != 0:
				if n > 0 {
					m.fail[r.Data.Title] = n - 1
				}
				return fmt.Errorf("the mail server refused %s", r.Data.Title)
			}
			m.sent = append(m.sent, r.Position)
			return nil
		}),
		events.Ignore(Renamed),
		events.Ignore(Archived),
	)
}

func (m *mailer) mailed() []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.sent)
}

func (m *mailer) set(title string, failures int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail[title] = failures
}

// position returns the position of a card's first event.
func (f *fixture) position(t testing.TB, id int64) int64 {
	t.Helper()
	recs := f.load(t, id)
	if len(recs) == 0 {
		t.Fatalf("card %d has no events", id)
	}
	return recs[0].Position
}

// A catch-up projector handles every event after the commit, woken by the
// writer: one that fails is retried with backoff, one that keeps failing
// parks the projector with its error, and Resume tries it again.
func TestCatchUp(t *testing.T) {
	m := newMailer()
	f := open(t, titles(1, same), m.projector(1))
	f.create(t, 1, "a")
	r := f.run(t, events.Options{Retries: 3, Backoff: time.Millisecond})
	waitFor(t, "the mail of the event before the start", func() bool { return len(m.mailed()) == 1 })

	m.set("flaky", 2)
	m.set("broken", -2)
	f.create(t, 2, "flaky")
	f.create(t, 3, "broken")
	f.create(t, 4, "d")
	waitFor(t, "the mails after retries", func() bool { return len(m.mailed()) == 4 })
	want := []int64{f.position(t, 1), f.position(t, 2), f.position(t, 3), f.position(t, 4)}
	if got := m.mailed(); !slices.Equal(got, want) {
		t.Errorf("mailed %v, want %v", got, want)
	}
	waitFor(t, "the checkpoint", func() bool {
		return f.count(t, `SELECT count(*) FROM lith_projections WHERE name = 'mailer' AND position = ?`, want[3]) == 1
	})
	if s := status(r, "mailer"); s.Status != "live" || s.Error != "" || s.Attempts != 0 || !s.CatchUp {
		t.Errorf("stats %+v", s)
	}

	m.set("poison", -1)
	f.create(t, 5, "poison")
	f.create(t, 6, "f")
	waitFor(t, "the projector parked", func() bool { return status(r, "mailer").Status == "parked" })
	s := status(r, "mailer")
	if !strings.Contains(s.Error, "refused poison") || s.Attempts != 3 || s.Position != want[3] {
		t.Errorf("parked: %+v", s)
	}
	if n := f.count(t, `SELECT count(*) FROM lith_projections WHERE name = 'mailer' AND status LIKE 'parked: %refused poison' AND position = ?`, want[3]); n != 1 {
		t.Error("lith_projections does not show the parked projector")
	}
	if n := len(m.mailed()); n != 4 {
		t.Errorf("%d mails while parked", n)
	}

	m.set("poison", 0)
	if err := r.Resume("mailer"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the mails after Resume", func() bool { return len(m.mailed()) == 6 })
	waitFor(t, "the projector live", func() bool {
		s := status(r, "mailer")
		return s.Status == "live" && s.Error == "" && s.Position == f.position(t, 6)
	})
	if err := r.Resume("card_titles"); err == nil {
		t.Error("Resume of an inline projector")
	}
}

// After a restart a catch-up projector goes on from its checkpoint, and a
// projector of a new version starts again from the first event, unless it
// skips history.
func TestCatchUpRestarts(t *testing.T) {
	m := newMailer()
	f := open(t, m.projector(1))
	ctx, cancel := context.WithCancel(context.Background())
	r := events.NewRunner(f.st, f.db, f.w, events.Options{Log: slog.New(slog.DiscardHandler)})
	done := make(chan error)
	go func() { done <- r.Run(ctx) }()
	f.createMany(t, 1, 3)
	waitFor(t, "three mails", func() bool { return len(m.mailed()) == 3 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.createMany(t, 4, 5)
	f.run(t, events.Options{})
	waitFor(t, "five mails", func() bool { return len(m.mailed()) == 5 })
	time.Sleep(20 * time.Millisecond)
	if got := m.mailed(); len(got) != 5 || !slices.IsSorted(got) {
		t.Errorf("mailed %v", got)
	}

	again := newMailer()
	v2 := f.reopen(again.projector(2))
	v2.run(t, events.Options{})
	waitFor(t, "the mails again", func() bool { return len(again.mailed()) == 5 })

	fresh := newMailer()
	v3 := f.reopen(fresh.projector(3).SkipHistory())
	v3.run(t, events.Options{})
	v3.create(t, 6, "new")
	waitFor(t, "the new mail", func() bool { return len(fresh.mailed()) == 1 })
	if got := fresh.mailed(); got[0] != f.position(t, 6) {
		t.Errorf("a projector that skips history mailed %v", got)
	}
}
