package board

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zweiundeins/lith/writer"
)

// Renamed is the event of a card's rename.
type Renamed struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// RenameCard renames a card, event-sourced: one Apply loads the card from
// its projection (the cards row), decides, appends the events and updates
// the projection. The writer applies one command at a time, so nothing
// changes between the read and the write, and no retry loop is needed.
// Renders read the projection, never the events.
type RenameCard struct {
	Card  int64
	Title string
	At    time.Time
}

func (c RenameCard) Apply(tx *writer.Tx) error {
	var card Card
	var board int64
	err := tx.QueryRow(`SELECT c.title, l.board_id FROM cards c JOIN lanes l ON l.id = c.lane_id
		WHERE c.id = ?`, c.Card).Scan(&card.Title, &board)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	events, err := decideRename(card, c.Title)
	if err != nil || len(events) == 0 {
		return err
	}
	for _, e := range events {
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO card_events (card_id, type, data, at) VALUES (?, 'renamed', ?, ?)`,
			c.Card, string(data), c.At.Unix()); err != nil {
			return err
		}
		card.Title = e.To
	}
	if _, err := tx.Exec(`UPDATE cards SET title = ?, version = version + 1 WHERE id = ?`, card.Title, c.Card); err != nil {
		return err
	}
	tx.Touch(Topic(board), CardTopic(c.Card))
	return nil
}

// decideRename is the decision alone: no database, so a test calls it
// without one.
func decideRename(card Card, title string) ([]Renamed, error) {
	title = strings.TrimSpace(title)
	switch {
	case title == "" || utf8.RuneCountInString(title) > MaxTitle:
		return nil, &writer.Refusal{Msg: fmt.Sprintf("A title has 1 to %d characters.", MaxTitle)}
	case title == card.Title:
		return nil, nil
	}
	return []Renamed{{From: card.Title, To: title}}, nil
}
