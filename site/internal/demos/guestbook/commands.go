package guestbook

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/writer"
)

// Sign adds an entry, unless the newest entry says the same or the message
// carries a link, and records the outcome in the state of the tab that
// sent it.
type Sign struct {
	Viewer        demo.Viewer
	Op            string
	Name, Message string
	At            time.Time
}

// Validate checks what the handler sets: no page of ours sends a signature
// without them. What the person typed, Apply checks, so that the refusal
// reaches the tab.
func (c Sign) Validate() error {
	if c.Viewer.Tab == "" || c.Op == "" {
		return errors.New("a signature needs a tab and an op")
	}
	return nil
}

func fits(s string, max int) bool {
	n := utf8.RuneCountInString(strings.TrimSpace(s))
	return n > 0 && n <= max
}

// links finds what a spammer posts; a guestbook on a public site needs no
// links.
var links = regexp.MustCompile(`(?i)https?://|www\.|\.(com|net|org|ru|xyz|top|io)\b`)

func (c Sign) Apply(tx *writer.Tx) error {
	name, message := clean(c.Name), clean(c.Message)
	state := TabState{Op: c.Op}
	var lastName, lastMessage string
	err := tx.QueryRow(`SELECT name, message FROM guestbook_entries ORDER BY id DESC LIMIT 1`).Scan(&lastName, &lastMessage)
	switch {
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	case !fits(name, MaxName):
		state.Refusal = fmt.Sprintf("A name has 1 to %d characters.", MaxName)
	case !fits(message, MaxMessage):
		state.Refusal = fmt.Sprintf("A message has 1 to %d characters.", MaxMessage)
	case links.MatchString(name + " " + message):
		state.Refusal = "Links stay out of this guestbook."
	case err == nil && lastName == name && lastMessage == message:
		state.Refusal = "That is the newest entry already."
	default:
		if _, err := tx.Exec(`INSERT INTO guestbook_entries (name, message, at) VALUES (?, ?, ?)`,
			name, message, c.At.Unix()); err != nil {
			return err
		}
		tx.Touch(Topic)
	}
	return demo.SaveTab(tx, Slug, c.Viewer, state, c.At)
}

// clean trims the text and drops control characters other than newlines.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// sweep keeps the newest Kept entries.
func sweep(tx *writer.Tx, now time.Time) error {
	n, err := demo.KeepNewest(tx, "guestbook_entries", Kept)
	if n > 0 {
		tx.Touch(Topic)
	}
	return err
}
