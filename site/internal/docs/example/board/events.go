package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"
)

// CardEvent is an event of a card's stream. A card itself is a row of
// cards, which AddCard makes; its stream holds its renames.
type CardEvent interface{ cardEvent() }

// Renamed is a card's rename.
type Renamed struct {
	Title string `json:"title"`
}

func (Renamed) cardEvent() {}

// Cards is the kind of the cards' streams: a stream per card, by its id.
var Cards = events.NewStream[int64, CardEvent]("card")

func defineCard[T CardEvent](name string) events.Type[int64, CardEvent, T] {
	return events.Define(Cards, name, func(t T) CardEvent { return t })
}

// CardRenamed is the type of Renamed, stored as card.renamed.
var CardRenamed = defineCard[Renamed]("card.renamed")

// titled is what a rename decides on: the title of the card's last
// rename, folded from its stream.
type titled struct {
	Title string
}

// Rename is the decider of RenameCard.
var Rename = events.Decider[RenameCard, CardEvent, titled]{
	Decide: decideRename,
	Evolve: func(s titled, e CardEvent) titled {
		switch e := e.(type) {
		case Renamed:
			s.Title = e.Title
		}
		return s
	},
	Version: 1,
}

// decideRename is the decision alone: no database, so a test runs it
// with eventstest.
func decideRename(c RenameCard, s titled) ([]CardEvent, error) {
	title := strings.TrimSpace(c.Title)
	switch {
	case title == "" || utf8.RuneCountInString(title) > MaxTitle:
		return nil, &writer.Refusal{Msg: fmt.Sprintf("A title has 1 to %d characters.", MaxTitle)}
	case title == s.Title:
		return nil, nil
	}
	return []CardEvent{Renamed{Title: title}}, nil
}

// titles keeps cards.title, which every board shows, in the rename's
// transaction. cards is not its own table (moves change it too), so it
// names no tables, and a new version rebuilds nothing.
var titles = events.NewProjector(Cards, "card_titles", 1,
	events.Handle(CardRenamed, func(tx *writer.Tx, r events.Record[int64, Renamed]) error {
		var board int64
		err := tx.QueryRow(`SELECT l.board_id FROM cards c JOIN lanes l ON l.id = c.lane_id WHERE c.id = ?`, r.ID).Scan(&board)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE cards SET title = ?, version = version + 1 WHERE id = ?`, r.Data.Title, r.ID); err != nil {
			return err
		}
		tx.Touch(Topic(board), CardTopic(r.ID))
		return nil
	}),
)

// history keeps card_history, every title a card had, for its panel. The
// table is the projector's own, so a new version of it is rebuilt from
// the events while pages read the old table.
var history = events.NewProjector(Cards, "card_history", 1,
	events.Handle(CardRenamed, func(tx *writer.Tx, r events.Record[int64, Renamed]) error {
		_, err := tx.Exec(`INSERT INTO card_history (card_id, version, title, at) VALUES (?, ?, ?, ?)`,
			r.ID, r.Version, r.Data.Title, r.At.Unix())
		tx.Touch(CardTopic(r.ID))
		return err
	}),
).Tables("card_history")

// Events is the board's event store. The app runs an events.Runner on it,
// which rebuilds a projector whose version changed.
var Events = events.Must(events.New(titles, history))

// RenameCard renames a card, event-sourced: Execute reads the card's
// state row, decides, appends the event, saves the row and runs both
// projectors, in this command's savepoint. A page shows a card's title,
// not its stream's version, so the last rename wins (events.Any); a page
// that shows the version sends it, and the command passes
// events.Exactly(c.Seen).
type RenameCard struct {
	Card  int64
	Title string
	At    time.Time
}

func (c RenameCard) Apply(tx *writer.Tx) error {
	var one int
	err := tx.QueryRow(`SELECT 1 FROM cards WHERE id = ?`, c.Card).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return events.Execute(Events, tx, Rename, Cards.ID(c.Card), events.Any, c, events.Meta{At: c.At})
}

// Title is one title a card had.
type Title struct {
	Version int64
	Title   string
	At      time.Time
}

// History reads a card's titles from card_history, oldest first.
func History(ctx context.Context, q sqlite.Querier, card int64) ([]Title, error) {
	rows, err := q.QueryContext(ctx, `SELECT version, title, at FROM card_history WHERE card_id = ? ORDER BY version`, card)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ts []Title
	for rows.Next() {
		var t Title
		var at int64
		if err := rows.Scan(&t.Version, &t.Title, &at); err != nil {
			return nil, err
		}
		t.At = time.Unix(at, 0)
		ts = append(ts, t)
	}
	return ts, rows.Err()
}
