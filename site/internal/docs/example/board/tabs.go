package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"
)

// TabState is a tab's own state, its row in tab_state: the op id of its
// last action, why that changed nothing if it did not, and the card whose
// panel is open. It is a row, not a map in memory, so it survives a
// deploy: the page reconnects to the next process and finds it there.
type TabState struct {
	Op      string `json:"op,omitempty"`
	Refusal string `json:"refusal,omitempty"`
	Panel   int64  `json:"panel,omitempty"`
}

// Tab reads a tab's state. A tab that never acted has no row yet, and
// its state is the zero value: a GET writes nothing.
func Tab(ctx context.Context, q sqlite.Querier, sid, tab string) (TabState, error) {
	var data string
	err := q.QueryRowContext(ctx, `SELECT data FROM tab_state WHERE sid = ? AND tab_id = ?`, sid, tab).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return TabState{}, nil
	}
	if err != nil {
		return TabState{}, err
	}
	var s TabState
	err = json.Unmarshal([]byte(data), &s)
	return s, err
}

// saveTab writes a tab's row and touches the tab's topic.
func saveTab(tx *writer.Tx, sid, tab string, s TabState, at time.Time) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO tab_state (sid, tab_id, data, seen) VALUES (?, ?, ?, ?)
		ON CONFLICT (sid, tab_id) DO UPDATE SET data = excluded.data, seen = excluded.seen`,
		sid, tab, string(data), at.Unix()); err != nil {
		return err
	}
	tx.Touch(TabTopic(tab))
	return nil
}

// OpenPanel opens a card's panel in a tab, or closes it with Card 0.
type OpenPanel struct {
	Session, Tab string
	Card         int64
	At           time.Time
}

func (c OpenPanel) Apply(tx *writer.Tx) error {
	s, err := Tab(tx.Context(), tx, c.Session, c.Tab)
	if err != nil {
		return err
	}
	s.Panel = c.Card
	return saveTab(tx, c.Session, c.Tab, s, c.At)
}

// Prefs are a session's preferences: they outlive the page and apply in
// every tab of the session.
type Prefs struct {
	Compact bool `json:"_compact"`
}

// SessionPrefs reads a session's preferences, the defaults for a session
// that never set one.
func SessionPrefs(ctx context.Context, q sqlite.Querier, sid string) (Prefs, error) {
	var p Prefs
	err := q.QueryRowContext(ctx, `SELECT compact FROM prefs WHERE sid = ?`, sid).Scan(&p.Compact)
	if errors.Is(err, sql.ErrNoRows) {
		return Prefs{}, nil
	}
	return p, err
}

// SetCompact sets the session's preference for compact cards.
type SetCompact struct {
	Session string
	Compact bool
	At      time.Time
}

func (c SetCompact) Apply(tx *writer.Tx) error {
	if _, err := tx.Exec(`INSERT INTO prefs (sid, compact, seen) VALUES (?, ?, ?)
		ON CONFLICT (sid) DO UPDATE SET compact = excluded.compact, seen = excluded.seen`,
		c.Session, c.Compact, c.At.Unix()); err != nil {
		return err
	}
	tx.Touch(SessionTopic(c.Session))
	return nil
}
