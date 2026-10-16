package events_test

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/writer"
)

// createMany creates cards from..to in one command.
func (f *fixture) createMany(t testing.TB, from, to int64) {
	t.Helper()
	if err := f.do(t, func(tx *writer.Tx) error {
		for id := from; id <= to; id++ {
			err := events.Execute(f.st, tx, create, Cards.ID(id), events.NoStream, Create{Title: fmt.Sprint("card ", id)}, events.Meta{})
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// tables lists the database's tables and indexes.
func (f *fixture) schema(t testing.TB) map[string]string {
	t.Helper()
	m := map[string]string{}
	rows, err := f.db.R.Query(`SELECT name, tbl_name FROM sqlite_schema WHERE type IN ('table', 'index')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, table string
		if err := rows.Scan(&name, &table); err != nil {
			t.Fatal(err)
		}
		m[name] = table
	}
	return m
}

// wantTitles checks that card_titles holds every card's title from its
// state row, through fn.
func (f *fixture) wantTitles(t testing.TB, fn func(string) string) {
	t.Helper()
	rows, err := f.db.R.Query(`SELECT stream_id, json_extract(state, '$.Title') FROM lith_event_state WHERE stream = 'card'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := f.titles(t)
	n := 0
	for rows.Next() {
		var id int64
		var title string
		if err := rows.Scan(&id, &title); err != nil {
			t.Fatal(err)
		}
		n++
		if got[id] != fn(title) {
			t.Fatalf("card %d is %q in card_titles, %q in its state", id, got[id], title)
		}
	}
	if n != len(got) || n == 0 {
		t.Fatalf("%d cards in card_titles, %d states", len(got), n)
	}
}

// A new version of a projector is rebuilt into a copy of its table while
// commands go on, and the copy takes the table's place at once: pages see
// the old table, with the new events on it, or the new one, never a copy
// half built. Views on the table read the new one after the swap.
func TestRebuild(t *testing.T) {
	f := open(t, titles(1, same))
	const cards = 1000
	f.createMany(t, 1, cards)
	if err := f.do(t, func(tx *writer.Tx) error {
		_, err := tx.ExecUncached(`CREATE VIEW card_count AS SELECT count(*) AS n FROM card_titles`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	v2 := f.reopen(titles(2, strings.ToUpper))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var reads, half, fresh, readErrs atomic.Int64
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				var n, old, newer, view int
				var last string
				err := f.db.View(context.Background(), func(tx *sql.Tx) error {
					err := tx.QueryRow(`SELECT count(*), count(*) FILTER (WHERE id <= ?), count(*) FILTER (WHERE id > ?),
						coalesce(max(title) FILTER (WHERE id = ?), '') FROM card_titles`, cards, cards, cards).Scan(&n, &old, &newer, &last)
					if err != nil {
						return err
					}
					return tx.QueryRow(`SELECT n FROM card_count`).Scan(&view)
				})
				switch {
				case err != nil:
					readErrs.Add(1)
					t.Log(err)
				case old != cards || view != n:
					half.Add(1)
				case newer > 0 && last == strings.ToLower(last):
					fresh.Add(1) // the old table, with cards made since
				}
				reads.Add(1)
			}
		})
	}
	var commands atomic.Int64
	wg.Go(func() {
		for i := int64(1); ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			err := v2.do(t, func(tx *writer.Tx) error {
				err := events.Execute(v2.st, tx, create, Cards.ID(cards+i), events.NoStream, Create{Title: fmt.Sprint("new ", i)}, events.Meta{})
				if err != nil {
					return err
				}
				return events.Execute(v2.st, tx, rename, Cards.ID(1+i%cards), events.Any, Rename{Title: fmt.Sprint("renamed ", i)}, events.Meta{})
			})
			if err != nil {
				t.Error(err)
				return
			}
			commands.Add(1)
		}
	})
	waitFor(t, "commands before the rebuild", func() bool { return commands.Load() > 20 })
	r := v2.run(t, events.Options{Batch: 50})
	waitFor(t, "the rebuild", func() bool { return status(r, "card_titles").Status == "live" })
	before := commands.Load()
	waitFor(t, "commands after the rebuild", func() bool { return commands.Load() > before+20 })
	close(stop)
	wg.Wait()

	if half.Load() != 0 || readErrs.Load() != 0 || fresh.Load() == 0 {
		t.Errorf("of %d reads, %d saw a table half rebuilt, %d failed, and %d saw new cards on the old table",
			reads.Load(), half.Load(), readErrs.Load(), fresh.Load())
	}
	v2.wantTitles(t, strings.ToUpper)
	schema := v2.schema(t)
	for name := range schema {
		if strings.HasPrefix(name, "card_titles__") {
			t.Errorf("%s is left", name)
		}
	}
	if schema["card_titles_title__v2"] != "card_titles" || schema["card_titles_title"] != "" {
		t.Errorf("the indexes: %v", schema)
	}
	if !contains(f.touchedAll(), "lith:projection:card_titles") {
		t.Error("the swap did not touch the projector's topic")
	}
	if n := v2.count(t, `SELECT count(*) FROM lith_projections WHERE name = 'card_titles' AND version = 2 AND status = 'live'`); n != 1 {
		t.Error("card_titles is not live at version 2")
	}
	t.Logf("%d commands ran during the rebuild and %d reads", commands.Load(), reads.Load())
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// touchedAll returns the topics committed since the last call, without
// sorting them.
func (f *fixture) touchedAll() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	got := f.topics
	f.topics = nil
	return got
}

// A rebuild that stops goes on where it stopped, with the next runner.
func TestRebuildResumes(t *testing.T) {
	f := open(t, titles(1, same))
	f.createMany(t, 1, 300)
	var applied atomic.Int64
	v2 := f.reopen(titles(2, func(s string) string { applied.Add(1); return strings.ToUpper(s) }))
	r := events.NewRunner(v2.st, v2.db, v2.w, events.Options{Batch: 1, Log: slog.New(slog.DiscardHandler)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- r.Run(ctx) }()
	waitFor(t, "a rebuild under way", func() bool { return status(r, "card_titles").Position >= 50 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var pos int64
	var st string
	if err := v2.db.R.QueryRow(`SELECT position, status FROM lith_projections WHERE name = 'card_titles'`).Scan(&pos, &st); err != nil {
		t.Fatal(err)
	}
	if st != "rebuilding" || pos < 50 || pos >= 300 {
		t.Fatalf("stopped at %d, %s", pos, st)
	}
	if v2.schema(t)["card_titles__v2"] == "" {
		t.Fatal("no copy to go on with")
	}
	again := v2.run(t, events.Options{Batch: 7})
	waitFor(t, "the rebuild", func() bool { return status(again, "card_titles").Status == "live" })
	v2.wantTitles(t, strings.ToUpper)
	if n := applied.Load(); n != 300 {
		t.Errorf("the handlers ran %d times for 300 events", n)
	}
}

// A projector added when its stream has events already is built from
// them; until then pages read its table empty.
func TestNewProjectorOnOldEvents(t *testing.T) {
	f := open(t)
	f.createMany(t, 1, 120)
	with := f.reopen(titles(1, same))
	if err := with.do(t, func(tx *writer.Tx) error {
		return events.Execute(with.st, tx, rename, Cards.ID(1), events.Any, Rename{Title: "first"}, events.Meta{})
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(with.titles(t)); n != 0 {
		t.Fatalf("%d titles before the build", n)
	}
	f.touched()
	r := with.run(t, events.Options{Batch: 50})
	waitFor(t, "the build", func() bool { return status(r, "card_titles").Status == "live" })
	with.wantTitles(t, same)
	// The batches fill a table no page reads: only the swap tells them.
	if got := f.touched(); !slices.Equal(got, []string{"lith:projection:card_titles"}) {
		t.Errorf("the build touched %v", got)
	}
}

// A projection table with a trigger cannot be copied: the rebuild says so
// and keeps the old table.
func TestRebuildRefusesTriggers(t *testing.T) {
	f := open(t, titles(1, same))
	f.createMany(t, 1, 3)
	if err := f.do(t, func(tx *writer.Tx) error {
		_, err := tx.ExecUncached(`CREATE TRIGGER card_titles_check BEFORE INSERT ON card_titles BEGIN SELECT 1; END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	v2 := f.reopen(titles(2, strings.ToUpper))
	r := v2.run(t, events.Options{Backoff: 1000000})
	waitFor(t, "the error", func() bool { return strings.Contains(status(r, "card_titles").Error, "trigger") })
	if got := v2.titles(t); got[1] != "card 1" {
		t.Errorf("card_titles %v", got)
	}
}

// While a projector rebuilds, the events of new commands reach its live
// table, so pages stay current, and its topics go out. A handler that
// fails there is logged and does not fail the command.
func TestRebuildKeepsPagesCurrent(t *testing.T) {
	f := open(t, titles(1, same))
	f.createMany(t, 1, 10)
	var logs strings.Builder
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logs.Write(p)
	}), nil)))
	defer slog.SetDefault(prev)

	v2 := f.reopen(titles(2, strings.ToUpper)) // no runner: it stays rebuilding
	v2.create(t, 11, "new")
	v2.rename(t, 1, "renamed")
	if got := v2.titles(t); got[11] != "NEW" || got[1] != "RENAMED" || got[2] != "card 2" {
		t.Errorf("card_titles during the rebuild: %v", got)
	}
	if got := f.touched(); !slices.Contains(got, "card:11") || !slices.Contains(got, "card:1") {
		t.Errorf("touched %v", got)
	}
	v2.create(t, 12, "fail")
	if got := v2.titles(t); got[12] != "" {
		t.Errorf("the failed handler's row is there: %v", got)
	}
	if n := v2.count(t, `SELECT count(*) FROM lith_events WHERE stream_id = 12`); n != 1 {
		t.Errorf("%d events of the command whose handler failed", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logs.String(), "failed on its live table") || !strings.Contains(logs.String(), "card_titles") {
		t.Errorf("logged %q", logs.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// A projector whose table is missing stops the commands of its own stream
// kind, with an error that names the table, and Run; other kinds go on.
func TestMissingTable(t *testing.T) {
	notes := events.NewProjector(Notes, "note_list", 1, events.Handle(Added, func(*writer.Tx, events.Record[int64, NoteAdded]) error {
		return nil
	})).Tables("note_list")
	f := open(t, titles(1, same), notes, upcastV1, upcastV2)
	f.create(t, 1, "a card")
	err := f.do(t, func(tx *writer.Tx) error {
		return events.Append(f.st, tx, Notes.ID(1), events.Any, events.Meta{}, events.One[NoteEvent](NoteAdded{Body: "x"}))
	})
	if err == nil || !strings.Contains(err.Error(), "no table note_list") {
		t.Errorf("a note without its table: %v", err)
	}
	f.rename(t, 1, "renamed")
	r := events.NewRunner(f.st, f.db, f.w, events.Options{Log: slog.New(slog.DiscardHandler)})
	if err := r.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "no table note_list") {
		t.Errorf("Run: %v", err)
	}
}
