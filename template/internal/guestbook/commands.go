package guestbook

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

// Sign adds an entry, unless it repeats the newest one, and records the
// outcome in the state of the tab that sent it.
type Sign struct {
	Session, Tab, Op string
	Name, Message    string
	At               time.Time
}

// Validate checks what the server sets: no page of ours sends a signature
// without them. What the person typed, Apply checks, so that its refusal
// reaches the tab.
func (c Sign) Validate() error {
	if c.Session == "" || c.Tab == "" || c.Op == "" {
		return errors.New("a signature needs a session, a tab and an op")
	}
	return nil
}

func fits(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return n > 0 && n <= max
}

func (c Sign) Apply(tx *writer.Tx) error {
	name, message := strings.TrimSpace(c.Name), strings.TrimSpace(c.Message)
	state := TabState{Op: c.Op}
	var lastName, lastMessage string
	err := tx.QueryRow(`SELECT name, message FROM entries ORDER BY id DESC LIMIT 1`).Scan(&lastName, &lastMessage)
	switch {
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	case !fits(name, MaxName):
		state.Refusal = fmt.Sprintf("A name has 1 to %d characters.", MaxName)
	case !fits(message, MaxMessage):
		state.Refusal = fmt.Sprintf("A message has 1 to %d characters.", MaxMessage)
	case err == nil && lastName == name && lastMessage == message:
		state.Refusal = "That is the newest entry already."
	default:
		if _, err := tx.Exec(`INSERT INTO entries (name, message, at) VALUES (?, ?, ?)`, name, message, c.At.Unix()); err != nil {
			return err
		}
		tx.Touch(Topic)
	}

	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO tab_state (sid, tab_id, data, seen) VALUES (?, ?, ?, ?)
		ON CONFLICT (sid, tab_id) DO UPDATE SET data = excluded.data, seen = excluded.seen`,
		c.Session, c.Tab, string(data), c.At.Unix()); err != nil {
		return err
	}
	tx.Touch(TabTopic(c.Tab))
	return nil
}

// Sweep keeps the tables bounded: it forgets the state of tabs that have not
// signed for TabTTL, and the entries beyond the newest Kept.
type Sweep struct{ At time.Time }

func (c Sweep) Apply(tx *writer.Tx) error {
	if _, err := tx.Exec(`DELETE FROM tab_state WHERE seen < ?`, c.At.Add(-TabTTL).Unix()); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM entries WHERE id <= (SELECT id FROM entries ORDER BY id DESC LIMIT 1 OFFSET ?)`, Kept)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		tx.Touch(Topic)
	}
	return nil
}
