package board

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// maxIndex bounds a move's place in a lane; the board has twelve cards.
const maxIndex = 64

// Move puts a card into a lane at a place, if the card is still at the
// version its mover saw, and records the outcome in the state of the tab
// that sent it.
type Move struct {
	Viewer demo.Viewer
	// Who is the mover's avatar seed (WhoOf of the session).
	Who  string
	Op   string
	Card int64
	// Lane and Index are where the card goes: Index counts the cards of
	// the lane without this one, so 0 is the top and the lane's length
	// its end.
	Lane, Index int
	// Seen is the card's version on the mover's page.
	Seen int64
	At   time.Time
}

func (c Move) Validate() error {
	switch {
	case c.Viewer.Tab == "" || c.Op == "":
		return errors.New("a move needs a tab and an op")
	case !validWho(c.Who):
		return errors.New("a move needs its mover")
	case c.Card <= 0:
		return errors.New("a move needs a card")
	case c.Lane < 0 || c.Lane >= Lanes:
		return fmt.Errorf("a lane is 0 to %d", Lanes-1)
	case c.Index < 0 || c.Index > maxIndex:
		return errors.New("there is no such place in a lane")
	case c.Seen < 0:
		return errors.New("a move needs the card's version")
	}
	return nil
}

func (c Move) Apply(tx *writer.Tx) error {
	var title string
	var lane, pos int
	var version int64
	var by string
	err := tx.QueryRow(`SELECT title, lane, pos, version, moved_by FROM board_cards WHERE id = ?`, c.Card).
		Scan(&title, &lane, &pos, &version, &by)
	state := TabState{Op: c.Op, Card: c.Card, Title: title, Lane: lane, Pos: pos}
	if errors.Is(err, sql.ErrNoRows) {
		state.Refusal = "That card is not on the board."
		if state.Version, err = boardVersion(tx); err != nil {
			return err
		}
		return demo.SaveTab(tx, Slug, c.Viewer, state, c.At)
	}
	if err != nil {
		return err
	}
	if version != c.Seen {
		// Someone moved the card after this page drew it: the later move
		// loses, and only its tab hears why.
		state.Refusal = refusal(title, lane, by, c.Who)
		if state.Version, err = boardVersion(tx); err != nil {
			return err
		}
		return demo.SaveTab(tx, Slug, c.Viewer, state, c.At)
	}
	var others int
	if err := tx.QueryRow(`SELECT count(*) FROM board_cards WHERE lane = ? AND id != ?`, c.Lane, c.Card).Scan(&others); err != nil {
		return err
	}
	index := min(c.Index, others)
	if lane == c.Lane && pos == index {
		// Dropped where it was: nothing changes.
		if state.Version, err = boardVersion(tx); err != nil {
			return err
		}
		return demo.SaveTab(tx, Slug, c.Viewer, state, c.At)
	}
	// Out of its lane, closing the gap, and into the other at index.
	if _, err := tx.Exec(`UPDATE board_cards SET pos = pos - 1 WHERE lane = ? AND pos > ?`, lane, pos); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE board_cards SET pos = pos + 1 WHERE lane = ? AND pos >= ? AND id != ?`, c.Lane, index, c.Card); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE board_cards SET lane = ?, pos = ?, version = version + 1, moved_by = ?, moved_at = ? WHERE id = ?`,
		c.Lane, index, c.Who, c.At.Unix(), c.Card); err != nil {
		return err
	}
	if err := tx.QueryRow(`UPDATE board_meta SET version = version + 1, moved_at = ?, last_card = ?, last_lane = ?, last_by = ? WHERE id = 1 RETURNING version`,
		c.At.Unix(), c.Card, c.Lane, c.Who).Scan(&state.Version); err != nil {
		return err
	}
	tx.Touch(Topic)
	state.Lane, state.Pos = c.Lane, index
	return demo.SaveTab(tx, Slug, c.Viewer, state, c.At)
}

// refusal says why a move lost: who moved the card first, and where it is.
func refusal(title string, lane int, by, me string) string {
	where := LaneNames[lane]
	switch by {
	case "":
		return fmt.Sprintf("The board went back to its start before your move arrived. “%s” is in %s.", title, where)
	case me:
		return fmt.Sprintf("You moved “%s” in another tab first. It is in %s now.", title, where)
	}
	return fmt.Sprintf("%s moved “%s” first. It is in %s now.", AvatarOf(by).Name, title, where)
}

func boardVersion(tx *writer.Tx) (v int64, err error) {
	err = tx.QueryRow(`SELECT version FROM board_meta WHERE id = 1`).Scan(&v)
	return v, err
}

// Hello records that a tab of the board's page is open, with the avatar
// seed of its session. It touches HereTopic when the tab is new to the
// table or its seed changed: the presence view then renders again.
type Hello struct {
	Viewer demo.Viewer
	Who    string
	At     time.Time
}

func (c Hello) Validate() error {
	if c.Viewer.Tab == "" || !validWho(c.Who) {
		return errors.New("a hello needs a tab and its session")
	}
	return nil
}

func (c Hello) Apply(tx *writer.Tx) error {
	var who string
	err := tx.QueryRow(`SELECT who FROM board_here WHERE tab = ?`, c.Viewer.Tab).Scan(&who)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.Exec(`INSERT INTO board_here (tab, who, since, at) VALUES (?, ?, ?, ?)`, c.Viewer.Tab, c.Who, c.At.Unix(), c.At.Unix()); err != nil {
			return err
		}
		tx.Touch(HereTopic)
		return nil
	case err != nil:
		return err
	}
	if _, err := tx.Exec(`UPDATE board_here SET who = ?, at = ? WHERE tab = ?`, c.Who, c.At.Unix(), c.Viewer.Tab); err != nil {
		return err
	}
	if who != c.Who {
		tx.Touch(HereTopic)
	}
	return nil
}

// sweep puts the cards back where they started once the board has been
// quiet for an hour, and forgets the hellos of tabs whose stream is gone.
func sweep(tx *writer.Tx, now time.Time) error {
	if err := resetIfQuiet(tx, now); err != nil {
		return err
	}
	return forgetGone(tx, now, streamOpen)
}

// resetIfQuiet restores the starting layout when the last move is Quiet
// ago. Every card it moves gets a new version, so a page that still shows
// the old board cannot move it.
func resetIfQuiet(tx *writer.Tx, now time.Time) error {
	var movedAt int64
	if err := tx.QueryRow(`SELECT moved_at FROM board_meta WHERE id = 1`).Scan(&movedAt); err != nil {
		return err
	}
	if movedAt == 0 || now.Sub(time.Unix(movedAt, 0)) < Quiet {
		return nil
	}
	if _, err := tx.Exec(`UPDATE board_cards SET lane = start_lane, pos = start_pos, version = version + 1, moved_by = '', moved_at = 0
		WHERE lane != start_lane OR pos != start_pos OR moved_by != ''`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE board_meta SET version = version + 1, moved_at = 0, last_card = 0, last_lane = 0, last_by = '' WHERE id = 1`); err != nil {
		return err
	}
	tx.Touch(Topic)
	return nil
}

// forgetGone deletes the hellos older than helloGrace whose tab has no
// open stream, and beyond the newest keptHellos any others.
func forgetGone(tx *writer.Tx, now time.Time, open func(tab string) bool) error {
	rows, err := tx.Query(`SELECT tab FROM board_here WHERE at < ?`, now.Add(-helloGrace).Unix())
	if err != nil {
		return err
	}
	var gone []string
	for rows.Next() {
		var tab string
		if err := rows.Scan(&tab); err != nil {
			rows.Close()
			return err
		}
		if !open(tab) {
			gone = append(gone, tab)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, tab := range gone {
		if _, err := tx.Exec(`DELETE FROM board_here WHERE tab = ?`, tab); err != nil {
			return err
		}
	}
	res, err := tx.Exec(`DELETE FROM board_here WHERE tab IN (SELECT tab FROM board_here ORDER BY at DESC LIMIT -1 OFFSET ?)`, keptHellos)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		tx.Touch(HereTopic)
	}
	return nil
}
