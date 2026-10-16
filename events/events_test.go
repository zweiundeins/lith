package events_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"
)

// The cards of the package doc: a stream per card, three event types,
// a decider per command and a projector into card_titles.

type CardEvent interface{ cardEvent() }

type CardCreated struct {
	Title string `json:"title"`
}

type CardRenamed struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type CardArchived struct{}

func (CardCreated) cardEvent()  {}
func (CardRenamed) cardEvent()  {}
func (CardArchived) cardEvent() {}

var Cards = events.NewStream[int64, CardEvent]("card")

func defineCard[T CardEvent](name string) events.Type[int64, CardEvent, T] {
	return events.Define(Cards, name, func(t T) CardEvent { return t })
}

var (
	Created  = defineCard[CardCreated]("card.created")
	Renamed  = defineCard[CardRenamed]("card.renamed")
	Archived = defineCard[CardArchived]("card.archived")
)

type card struct {
	Exists   bool
	Title    string
	Archived bool
}

func evolveCard(s card, e CardEvent) card {
	switch e := e.(type) {
	case CardCreated:
		return card{Exists: true, Title: e.Title}
	case CardRenamed:
		s.Title = e.To
	case CardArchived:
		s.Archived = true
	}
	return s
}

var errNotFound = errors.New("no such card")

type Create struct{ Title string }
type Rename struct{ Title string }
type Archive struct{}

// cardDecider is a decider of the card stream, sharing evolveCard.
func cardDecider[C any](decide func(C, card) ([]CardEvent, error)) events.Decider[C, CardEvent, card] {
	return events.Decider[C, CardEvent, card]{Decide: decide, Evolve: evolveCard, Version: 1}
}

var (
	create = cardDecider(func(c Create, s card) ([]CardEvent, error) {
		if s.Exists {
			return nil, errors.New("the card exists")
		}
		return []CardEvent{CardCreated{Title: c.Title}}, nil
	})
	rename = cardDecider(func(c Rename, s card) ([]CardEvent, error) {
		switch {
		case !s.Exists:
			return nil, errNotFound
		case c.Title == s.Title:
			return nil, nil
		}
		return []CardEvent{CardRenamed{From: s.Title, To: c.Title}}, nil
	})
	archive = cardDecider(func(c Archive, s card) ([]CardEvent, error) {
		if !s.Exists {
			return nil, errNotFound
		}
		if s.Archived {
			return nil, nil
		}
		return []CardEvent{CardArchived{}}, nil
	})
)

func cardTopic(id int64) string { return fmt.Sprint("card:", id) }

// errFail is what the titles projector returns for the title "fail".
var errFail = errors.New("the projector failed")

// titles projects the cards into card_titles, the titles through f.
func titles(version int, f func(string) string) *events.Projector[int64, CardEvent] {
	return events.NewProjector(Cards, "card_titles", version,
		events.Handle(Created, func(tx *writer.Tx, r events.Record[int64, CardCreated]) error {
			if r.Data.Title == "fail" {
				return errFail
			}
			_, err := tx.Exec(`INSERT INTO card_titles (id, title) VALUES (?, ?)`, r.ID, f(r.Data.Title))
			tx.Touch(cardTopic(r.ID))
			return err
		}),
		events.Handle(Renamed, func(tx *writer.Tx, r events.Record[int64, CardRenamed]) error {
			_, err := tx.Exec(`UPDATE card_titles SET title = ? WHERE id = ?`, f(r.Data.To), r.ID)
			tx.Touch(cardTopic(r.ID))
			return err
		}),
		events.Handle(Archived, func(tx *writer.Tx, r events.Record[int64, CardArchived]) error {
			_, err := tx.Exec(`UPDATE card_titles SET archived = 1 WHERE id = ?`, r.ID)
			return err
		}),
	).Tables("card_titles")
}

func same(s string) string { return s }

const schema = `
CREATE TABLE card_titles (
	id       INTEGER PRIMARY KEY,
	title    TEXT NOT NULL,
	archived INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX card_titles_title ON card_titles (title);
`

type fixture struct {
	db     *sqlite.DB
	w      *writer.Writer
	st     *events.Store
	mu     sync.Mutex
	topics []string
}

// open opens a database with card_titles, runs a writer on it and makes a
// store of parts.
func open(t testing.TB, parts ...events.Part) *fixture {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "events.db"), sqlite.Options{
		Migrations: fstest.MapFS{"001_cards.sql": {Data: []byte(schema)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{db: db}
	f.w, err = writer.New(db, writer.Options{
		Log: slog.New(slog.DiscardHandler),
		OnCommit: func(ts []string) {
			f.mu.Lock()
			f.topics = append(f.topics, ts...)
			f.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go f.w.Run(context.Background())
	t.Cleanup(func() {
		f.w.Close()
		db.Close()
	})
	f.st = events.Must(events.New(parts...))
	return f
}

// reopen makes a new store of parts on the same database, as a new
// version of the app does.
func (f *fixture) reopen(parts ...events.Part) *fixture {
	return &fixture{db: f.db, w: f.w, st: events.Must(events.New(parts...))}
}

// touched returns the topics committed since the last call.
func (f *fixture) touched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	got := f.topics
	f.topics = nil
	slices.Sort(got)
	return slices.Compact(got)
}

func (f *fixture) do(t testing.TB, fn func(tx *writer.Tx) error) error {
	t.Helper()
	return f.w.Do(context.Background(), writer.Func(fn))
}

func (f *fixture) create(t testing.TB, id int64, title string) {
	t.Helper()
	if err := f.do(t, func(tx *writer.Tx) error {
		return events.Execute(f.st, tx, create, Cards.ID(id), events.NoStream, Create{Title: title}, events.Meta{})
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) rename(t testing.TB, id int64, title string) {
	t.Helper()
	if err := f.do(t, func(tx *writer.Tx) error {
		return events.Execute(f.st, tx, rename, Cards.ID(id), events.Any, Rename{Title: title}, events.Meta{})
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) count(t testing.TB, q string, args ...any) int {
	t.Helper()
	n, err := f.db.ViewValue(context.Background(), func(tx *sql.Tx) (n int, err error) {
		err = tx.QueryRow(q, args...).Scan(&n)
		return n, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// titlesOf reads card_titles as id: title.
func (f *fixture) titles(t testing.TB) map[int64]string {
	t.Helper()
	m, err := f.db.ViewValue(context.Background(), func(tx *sql.Tx) (map[int64]string, error) {
		rows, err := tx.Query(`SELECT id, title FROM card_titles`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		m := map[int64]string{}
		for rows.Next() {
			var id int64
			var title string
			if err := rows.Scan(&id, &title); err != nil {
				return nil, err
			}
			m[id] = title
		}
		return m, rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// load reads a card's events through a read transaction.
func (f *fixture) load(t testing.TB, id int64) []events.Record[int64, CardEvent] {
	t.Helper()
	ctx := context.Background()
	recs, err := f.db.ViewValue(ctx, func(tx *sql.Tx) ([]events.Record[int64, CardEvent], error) {
		var recs []events.Record[int64, CardEvent]
		for r, err := range events.Load(ctx, f.st, tx, Cards.ID(id), 0) {
			if err != nil {
				return nil, err
			}
			recs = append(recs, r)
		}
		return recs, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("no %s after 10s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// run starts a runner until the test ends.
func (f *fixture) run(t testing.TB, opts events.Options) *events.Runner {
	t.Helper()
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	r := events.NewRunner(f.st, f.db, f.w, opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return r
}

func status(r *events.Runner, name string) events.ProjectorStats {
	for _, p := range r.Stats().Projectors {
		if p.Name == name {
			return p
		}
	}
	return events.ProjectorStats{}
}

// A command executes on a stream's state row: the events, the row and the
// projection commit together, with the record's metadata, and the
// projector's and the module's topics.
func TestExecute(t *testing.T) {
	f := open(t, titles(1, same))
	at := time.UnixMilli(1_800_000_000_123)
	if err := f.do(t, func(tx *writer.Tx) error {
		return events.Execute(f.st, tx, create, Cards.ID(7), events.NoStream, Create{Title: "Draft"},
			events.Meta{At: at, Actor: "ada", Correlation: "req-1", Causation: "op-1"})
	}); err != nil {
		t.Fatal(err)
	}
	f.rename(t, 7, "Final")
	f.rename(t, 7, "Final") // decides nothing
	if got := f.titles(t); got[7] != "Final" || len(got) != 1 {
		t.Errorf("card_titles %v", got)
	}
	recs := f.load(t, 7)
	if len(recs) != 2 {
		t.Fatalf("%d events", len(recs))
	}
	first, second := recs[0], recs[1]
	if first.Version != 1 || second.Version != 2 || second.Position <= first.Position || first.ID != 7 {
		t.Errorf("records %+v", recs)
	}
	if first.Type != "card.created" || first.Data != (CardCreated{Title: "Draft"}) || second.Data != (CardRenamed{From: "Draft", To: "Final"}) {
		t.Errorf("records %+v", recs)
	}
	if first.Meta != (events.Meta{At: at, Actor: "ada", Correlation: "req-1", Causation: "op-1"}) {
		t.Errorf("meta %+v", first.Meta)
	}
	if d := time.Since(second.At); d < 0 || d > time.Minute {
		t.Errorf("an event without a time got %v", second.At)
	}
	if n := f.count(t, `SELECT count(*) FROM lith_event_state WHERE stream = 'card' AND stream_id = 7 AND version = 2 AND fold = 1`); n != 1 {
		t.Error("no state row at version 2")
	}
	if got := f.touched(); !slices.Equal(got, []string{"card:7", events.Topic}) {
		t.Errorf("touched %v", got)
	}
	if err := f.do(t, func(tx *writer.Tx) error {
		return events.Execute(f.st, tx, rename, Cards.ID(8), events.Any, Rename{Title: "x"}, events.Meta{})
	}); !errors.Is(err, errNotFound) {
		t.Errorf("a rename of no card: %v", err)
	}
}

// The expected version is checked before the decision; a conflict changes
// nothing.
func TestExpectedVersion(t *testing.T) {
	f := open(t, titles(1, same))
	exec := func(want events.Expected, title string) error {
		return f.do(t, func(tx *writer.Tx) error {
			return events.Execute(f.st, tx, rename, Cards.ID(1), want, Rename{Title: title}, events.Meta{})
		})
	}
	f.create(t, 1, "a")
	for _, c := range []struct {
		want     events.Expected
		conflict bool
	}{
		{events.NoStream, true},
		{events.Exactly(0), true},
		{events.Exactly(2), true},
		{events.Exactly(1), false}, // now at 2
		{events.Exactly(1), true},
		{events.Exactly(2), false},
		{events.Any, false},
	} {
		before := f.count(t, `SELECT count(*) FROM lith_events`)
		err := exec(c.want, fmt.Sprint("title ", before))
		if errors.Is(err, events.ErrConflict) != c.conflict || err != nil && !c.conflict {
			t.Fatalf("%+v at %d events: %v", c.want, before, err)
		}
		if n := f.count(t, `SELECT count(*) FROM lith_events`); c.conflict && n != before {
			t.Fatalf("a conflict appended %d events", n-before)
		}
	}
	err := f.do(t, func(tx *writer.Tx) error {
		return events.Append(f.st, tx, Cards.ID(1), events.Exactly(3), events.Meta{}, events.One[CardEvent](CardArchived{}))
	})
	if !errors.Is(err, events.ErrConflict) {
		t.Errorf("Append at a stale version: %v", err)
	}
}

// Append appends without a decision. It drops the stream's state row, and
// the next Execute folds the stream again, its appended events included.
func TestAppend(t *testing.T) {
	f := open(t, titles(1, same))
	f.create(t, 1, "a")
	err := f.do(t, func(tx *writer.Tx) error {
		return events.Append(f.st, tx, Cards.ID(1), events.Exactly(1), events.Meta{Actor: "import"},
			[]CardEvent{CardRenamed{From: "a", To: "b"}, CardArchived{}}...)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM lith_event_state`); n != 0 {
		t.Errorf("%d state rows after Append", n)
	}
	if err := f.do(t, func(tx *writer.Tx) error {
		return events.Execute(f.st, tx, archive, Cards.ID(1), events.Exactly(3), Archive{}, events.Meta{})
	}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM lith_events`); n != 3 {
		t.Errorf("the archive appended again: %d events", n)
	}
	if got := f.titles(t); got[1] != "b" {
		t.Errorf("card_titles %v", got)
	}
	type Other struct{ CardEvent }
	if err := f.do(t, func(tx *writer.Tx) error {
		return events.Append(f.st, tx, Cards.ID(1), events.Any, events.Meta{}, CardEvent(Other{}))
	}); err == nil || !strings.Contains(err.Error(), "not an event type") {
		t.Errorf("an undefined type: %v", err)
	}
}

type UserID string

type UserEvent interface{ userEvent() }

type UserJoined struct {
	Name string `json:"name"`
}

func (UserJoined) userEvent() {}

var (
	Users  = events.NewStream[UserID, UserEvent]("user")
	Joined = events.Define(Users, "user.joined", func(e UserJoined) UserEvent { return e })
)

// Keys of a named string type go in and come out as they are, apart from
// the integer keys of other streams.
func TestStringKeys(t *testing.T) {
	var seen []UserID
	users := events.NewProjector(Users, "users", 1,
		events.Handle(Joined, func(tx *writer.Tx, r events.Record[UserID, UserJoined]) error {
			seen = append(seen, r.ID)
			return nil
		}))
	f := open(t, users)
	for _, id := range []UserID{"ada", "7"} {
		if err := f.do(t, func(tx *writer.Tx) error {
			return events.Append(f.st, tx, Users.ID(id), events.NoStream, events.Meta{}, events.One[UserEvent](UserJoined{Name: string(id)}))
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.create(t, 7, "a card")
	if !slices.Equal(seen, []UserID{"ada", "7"}) {
		t.Errorf("handled %v", seen)
	}
	ctx := context.Background()
	err := f.db.View(ctx, func(tx *sql.Tx) error {
		for r, err := range events.Load(ctx, f.st, tx, Users.ID("7"), 0) {
			if err != nil {
				return err
			}
			if r.ID != "7" || r.Data != (UserJoined{Name: "7"}) {
				t.Errorf("record %+v", r)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM lith_events WHERE stream_id = 7`); n != 1 {
		t.Errorf("%d events with the integer key 7", n)
	}
}

// New refuses what would fail at run time.
func TestNewRefuses(t *testing.T) {
	handles := []events.Handler[int64, CardEvent]{events.Ignore(Created), events.Ignore(Renamed), events.Ignore(Archived)}
	other := events.NewStream[int64, CardEvent]("card")
	afterCreated := events.After(Created, func(context.Context, events.Record[int64, CardCreated]) error { return nil })
	for name, parts := range map[string][]events.Part{
		"incomplete": {events.NewProjector(Cards, "p", 1, events.Ignore(Created), events.Ignore(Renamed))},
		"twice":      {events.NewProjector(Cards, "p", 1, append(handles, events.Ignore(Created))...)},
		"same name":  {events.NewProjector(Cards, "p", 1, handles...), events.NewProjector(Cards, "p", 1, handles...)},
		"same kind":  {events.NewProjector(Cards, "p", 1, handles...), events.NewProjector(other, "q", 1)},
		"no name":    {events.NewProjector(Cards, "", 1, handles...)},
		"mixed": {events.NewProjector(Cards, "p", 1, events.Ignore(Archived), events.Ignore(Renamed), afterCreated,
			events.Handle(Archived, func(*writer.Tx, events.Record[int64, CardArchived]) error { return nil }))},
		"catch-up tables": {events.NewProjector(Cards, "p", 1, events.Ignore(Archived), events.Ignore(Renamed), afterCreated).Tables("t")},
		"inline history":  {events.NewProjector(Cards, "p", 1, handles...).SkipHistory()},
		"one table": {
			events.NewProjector(Cards, "p", 1, handles...).Tables("card_titles"),
			events.NewProjector(Cards, "q", 1, handles...).Tables("Card_Titles"),
		},
		"self upcast": {events.Upcast(Created, Created, func(c CardCreated) CardCreated { return c })},
		"upcast twice": {
			events.Upcast(Created, Renamed, func(CardCreated) CardRenamed { return CardRenamed{} }),
			events.Upcast(Created, Archived, func(CardCreated) CardArchived { return CardArchived{} }),
		},
		"handles an upcast type": {
			events.Upcast(Archived, Renamed, func(CardArchived) CardRenamed { return CardRenamed{} }),
			events.NewProjector(Cards, "p", 1, handles...),
		},
		"circle": {
			events.Upcast(Created, Renamed, func(CardCreated) CardRenamed { return CardRenamed{} }),
			events.Upcast(Renamed, Created, func(CardRenamed) CardCreated { return CardCreated{} }),
		},
	} {
		if _, err := events.New(parts...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	defer func() {
		if p := recover(); p == nil || !strings.Contains(fmt.Sprint(p), "neither handles nor ignores card.archived") {
			t.Errorf("Must: %v", p)
		}
	}()
	events.Must(events.New(events.NewProjector(Cards, "p", 1, events.Ignore(Created), events.Ignore(Renamed))))
}

// The deciders of a stream kind share their state: Execute refuses one
// whose state or Version differs.
func TestDecidersShareTheirState(t *testing.T) {
	f := open(t)
	f.create(t, 1, "a")
	other := rename
	other.Version = 2
	err := f.do(t, func(tx *writer.Tx) error {
		return events.Execute(f.st, tx, other, Cards.ID(1), events.Any, Rename{Title: "b"}, events.Meta{})
	})
	if err == nil || !strings.Contains(err.Error(), "differ") {
		t.Errorf("a decider of another version: %v", err)
	}
}

// A decider with a new Version folds a stream again at its next command,
// from its events, and keeps the new state in the row.
func TestRefold(t *testing.T) {
	f := open(t, titles(1, same))
	f.create(t, 1, "draft")
	f.rename(t, 1, "final")
	upper := func(s card, e CardEvent) card {
		s = evolveCard(s, e)
		s.Title = strings.ToUpper(s.Title)
		return s
	}
	v2 := f.reopen(titles(1, same))
	rename2 := rename
	rename2.Evolve, rename2.Version = upper, 2
	for _, title := range []string{"FINAL", "shipped"} {
		if err := v2.do(t, func(tx *writer.Tx) error {
			return events.Execute(v2.st, tx, rename2, Cards.ID(1), events.Exactly(2), Rename{Title: title}, events.Meta{})
		}); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	err := f.db.View(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT state FROM lith_event_state WHERE stream = 'card' AND stream_id = 1 AND fold = 2 AND version = 3`).Scan(&state)
	})
	if err != nil || state != `{"Exists":true,"Title":"SHIPPED","Archived":false}` {
		t.Errorf("state %s, %v", state, err)
	}
}

// A store's reads see no events, and no error, before the first command
// made the tables.
func TestReadsBeforeTheTables(t *testing.T) {
	f := open(t)
	if recs := f.load(t, 1); len(recs) != 0 {
		t.Errorf("%d records", len(recs))
	}
	ctx := context.Background()
	err := f.db.View(ctx, func(tx *sql.Tx) error {
		for _, err := range events.Since(ctx, tx, 0) {
			return err
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}
}
