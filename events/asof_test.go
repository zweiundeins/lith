package events_test

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math/rand/v2"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"
)

// titleHistory keeps every version of a card's title in title_history,
// with the positions between which it held: the card as of position p is
// its row with valid_from <= p < valid_to. A change ends the card's
// current row at the event's position and starts the next row there.
var titleHistory = events.NewProjector(Cards, "title_history", 1,
	events.Handle(Created, func(tx *writer.Tx, r events.Record[int64, CardCreated]) error {
		_, err := tx.Exec(`INSERT INTO title_history (card_id, title, valid_from) VALUES (?, ?, ?)`, r.ID, r.Data.Title, r.Position)
		return err
	}),
	events.Handle(Renamed, func(tx *writer.Tx, r events.Record[int64, CardRenamed]) error {
		if err := endRow(tx, r.ID, r.Position); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO title_history (card_id, title, archived, valid_from)
			SELECT card_id, ?3, archived, ?1 FROM title_history WHERE card_id = ?2 AND valid_to = ?1`, r.Position, r.ID, r.Data.To)
		return err
	}),
	events.Handle(Archived, func(tx *writer.Tx, r events.Record[int64, CardArchived]) error {
		if err := endRow(tx, r.ID, r.Position); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO title_history (card_id, title, archived, valid_from)
			SELECT card_id, title, 1, ?1 FROM title_history WHERE card_id = ?2 AND valid_to = ?1`, r.Position, r.ID)
		return err
	}),
).Tables("title_history")

// endRow ends a card's current row at position pos.
func endRow(tx *writer.Tx, card, pos int64) error {
	_, err := tx.Exec(`UPDATE title_history SET valid_to = ? WHERE card_id = ? AND valid_to IS NULL`, pos, card)
	return err
}

// titlesAt returns every card's title as of position pos.
func titlesAt(ctx context.Context, q sqlite.Querier, pos int64) (map[int64]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT card_id, title FROM title_history
		WHERE valid_from <= ?1 AND (valid_to IS NULL OR valid_to > ?1)`, pos)
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
}

// replayAt folds every event up to position pos into the cards' titles.
func replayAt(ctx context.Context, st *events.Store, q sqlite.Querier, pos int64) (map[int64]string, error) {
	cards := map[int64]card{}
	for raw, err := range events.SinceUntil(ctx, q, 0, pos) {
		if err != nil {
			return nil, err
		}
		if raw.Stream != Cards.Kind() {
			continue
		}
		e, err := events.Decode(st, Cards, raw.Type, raw.Data)
		if err != nil {
			return nil, err
		}
		id := raw.ID.(int64)
		cards[id] = evolveCard(cards[id], e)
	}
	m := map[int64]string{}
	for id, c := range cards {
		if c.Exists {
			m[id] = c.Title
		}
	}
	return m, nil
}

// The three ways to look back agree at every position: the history
// table, a replay of the log up to the position, and each card's state
// folded up to it.
func TestAsOf(t *testing.T) {
	f := open(t, titleHistory)
	rnd := rand.New(rand.NewPCG(3, 4))
	for range 200 {
		id := 1 + rnd.Int64N(10)
		f.do(t, func(tx *writer.Tx) error {
			switch rnd.IntN(4) {
			case 0:
				return events.Execute(f.st, tx, create, Cards.ID(id), events.Any, Create{Title: fmt.Sprint("t", rnd.IntN(5))}, events.Meta{})
			case 3:
				return events.Execute(f.st, tx, archive, Cards.ID(id), events.Any, Archive{}, events.Meta{})
			}
			return events.Execute(f.st, tx, rename, Cards.ID(id), events.Any, Rename{Title: fmt.Sprint("t", rnd.IntN(5))}, events.Meta{})
		})
	}
	ctx := context.Background()
	head := f.maxPosition(t)
	err := f.db.View(ctx, func(tx *sql.Tx) error {
		for pos := range head + 1 {
			table, err := titlesAt(ctx, tx, pos)
			if err != nil {
				return err
			}
			replayed, err := replayAt(ctx, f.st, tx, pos)
			if err != nil {
				return err
			}
			if !maps.Equal(table, replayed) {
				return fmt.Errorf("at %d the history table has %v, the replay %v", pos, table, replayed)
			}
			for id := range int64(10) {
				s, _, err := events.StateAt(ctx, f.st, tx, rename, Cards.ID(id+1), pos)
				if err != nil {
					return err
				}
				if title, ok := table[id+1]; ok != s.Exists || title != s.Title && ok {
					return fmt.Errorf("at %d card %d is %+v, in the history table %q", pos, id+1, s, title)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// BenchmarkAsOf is a page that looks back to a position of a log of
// 10,000 events, 100 cards each created and renamed 99 times: the cards'
// titles from the history table, the same from a replay of the log, and
// one card's state from its stream. Each op is one read transaction, as a
// render has, at a random position.
//
//	go test ./events -run '^$' -bench AsOf
func BenchmarkAsOf(b *testing.B) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(b.TempDir(), "asof.db"), sqlite.Options{
		Migrations: fstest.MapFS{"001_cards.sql": {Data: []byte(schema)}},
	})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	w, err := writer.New(db, writer.Options{})
	if err != nil {
		b.Fatal(err)
	}
	go w.Run(ctx)
	defer w.Close()
	st := events.Must(events.New(titleHistory))
	err = w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
		for id := range int64(100) {
			if err := events.Execute(st, tx, create, Cards.ID(id+1), events.NoStream, Create{Title: "card"}, events.Meta{}); err != nil {
				return err
			}
		}
		for i := range 9900 {
			id := Cards.ID(int64(1 + i%100))
			if err := events.Execute(st, tx, rename, id, events.Any, Rename{Title: fmt.Sprint("title ", i)}, events.Meta{}); err != nil {
				return err
			}
		}
		return nil
	}))
	if err != nil {
		b.Fatal(err)
	}
	for name, look := range map[string]func(tx *sql.Tx, pos int64) error{
		"history-table": func(tx *sql.Tx, pos int64) error { _, err := titlesAt(ctx, tx, pos); return err },
		"replay":        func(tx *sql.Tx, pos int64) error { _, err := replayAt(ctx, st, tx, pos); return err },
		"state-of-one-card": func(tx *sql.Tx, pos int64) error {
			_, _, err := events.StateAt(ctx, st, tx, rename, Cards.ID(1+pos%100), pos)
			return err
		},
	} {
		b.Run(name, func(b *testing.B) {
			rnd := rand.New(rand.NewPCG(1, 2))
			for b.Loop() {
				pos := 1 + rnd.Int64N(10_000)
				if err := db.View(ctx, func(tx *sql.Tx) error { return look(tx, pos) }); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
