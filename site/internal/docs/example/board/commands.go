package board

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/writer"
)

// CreateBoard creates a board with three lanes.
type CreateBoard struct{ Name string }

func (c CreateBoard) Validate() error {
	if n := utf8.RuneCountInString(strings.TrimSpace(c.Name)); n == 0 || n > MaxTitle {
		return fmt.Errorf("a name has 1 to %d characters", MaxTitle)
	}
	return nil
}

// Insert creates the board and returns its id.
func (c CreateBoard) Insert(tx *writer.Tx) (int64, error) {
	res, err := tx.Exec(`INSERT INTO boards (name) VALUES (?)`, strings.TrimSpace(c.Name))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, lane := range []string{"To do", "Doing", "Done"} {
		if _, err := tx.Exec(`INSERT INTO lanes (board_id, name) VALUES (?, ?)`, id, lane); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// ApplyResult creates the board and answers its id, for writer.OnceResult.
func (c CreateBoard) ApplyResult(tx *writer.Tx) ([]byte, error) {
	id, err := c.Insert(tx)
	if err != nil {
		return nil, err
	}
	return strconv.AppendInt(nil, id, 10), nil
}

// Create creates a board and returns its id, for a test, a tool or a new
// account. DoValue takes a function, not a command, so the command's
// Validate does not run unless Create calls it.
func Create(ctx context.Context, w *writer.Writer, name string) (int64, error) {
	c := CreateBoard{Name: name}
	if err := c.Validate(); err != nil {
		return 0, err
	}
	return w.DoValue(ctx, c.Insert)
}

// AddCard adds a card at the end of a lane.
type AddCard struct {
	Lane  int64
	Title string
}

// Validate runs before the command is queued: Send and Do return its
// error wrapped in writer.ErrInvalid, and the writer never sees it.
func (c AddCard) Validate() error {
	if c.Lane <= 0 {
		return errors.New("a card needs a lane")
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(c.Title)); n == 0 || n > MaxTitle {
		return fmt.Errorf("a title has 1 to %d characters", MaxTitle)
	}
	return nil
}

func (c AddCard) Apply(tx *writer.Tx) error {
	_, err := c.Insert(tx)
	return err
}

// Insert adds the card and returns its id. A full lane refuses it.
func (c AddCard) Insert(tx *writer.Tx) (int64, error) {
	var board int64
	var limit int
	err := tx.QueryRow(`SELECT board_id, max_cards FROM lanes WHERE id = ?`, c.Lane).Scan(&board, &limit)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	n, err := LaneCount(tx.Context(), tx, c.Lane) // the same query a GET and a view run
	if err != nil {
		return 0, err
	}
	if n >= limit {
		return 0, &writer.Refusal{Msg: "This lane is full."}
	}
	res, err := tx.Exec(`INSERT INTO cards (lane_id, title) VALUES (?, ?)`, c.Lane, strings.TrimSpace(c.Title))
	if err != nil {
		return 0, err
	}
	tx.Touch(Topic(board))
	return res.LastInsertId()
}

// ApplyResult adds the card and answers its id, for writer.OnceResult.
func (c AddCard) ApplyResult(tx *writer.Tx) ([]byte, error) {
	id, err := c.Insert(tx)
	if err != nil {
		return nil, err
	}
	return strconv.AppendInt(nil, id, 10), nil
}

// MoveCard moves a card to another lane of its board. Seen is the card's
// version that the page showed: a card that moved meanwhile stays where
// it is, and the page says why.
type MoveCard struct{ Card, Lane, Seen int64 }

func (c MoveCard) Validate() error {
	if c.Card <= 0 || c.Lane <= 0 {
		return errors.New("a move needs a card and a lane")
	}
	return nil
}

func (c MoveCard) Apply(tx *writer.Tx) error {
	var board, version int64
	err := tx.QueryRow(`SELECT l.board_id, c.version FROM cards c JOIN lanes l ON l.id = c.lane_id
		WHERE c.id = ? AND c.archived IS NULL`, c.Card).Scan(&board, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if version != c.Seen {
		return &writer.Refusal{Msg: "Someone moved this card meanwhile."}
	}
	res, err := tx.Exec(`UPDATE cards SET lane_id = ?, version = version + 1
		WHERE id = ? AND EXISTS (SELECT 1 FROM lanes WHERE id = ? AND board_id = ?)`, c.Lane, c.Card, c.Lane, board)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return cmp.Or(err, ErrNotFound) // no such lane on the card's board
	}
	tx.Touch(Topic(board), CardTopic(c.Card))
	return nil
}

// ArchiveCards takes cards off a board. Its SQL has a mark per card, so
// it runs uncached: each length of the list would otherwise take a place
// in the writer's statement cache.
type ArchiveCards struct {
	Board int64
	Cards []int64
	At    time.Time
}

func (c ArchiveCards) Validate() error {
	if len(c.Cards) == 0 || len(c.Cards) > 500 {
		return errors.New("archive 1 to 500 cards at a time")
	}
	return nil
}

func (c ArchiveCards) Apply(tx *writer.Tx) error {
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(c.Cards)), ", ")
	args := []any{c.At.Unix(), c.Board}
	for _, id := range c.Cards {
		args = append(args, id)
	}
	_, err := tx.ExecUncached(`UPDATE cards SET archived = ?
		WHERE archived IS NULL AND lane_id IN (SELECT id FROM lanes WHERE board_id = ?)
		AND id IN (`+marks+`)`, args...)
	if err != nil {
		return err
	}
	tx.Touch(Topic(c.Board))
	return nil
}

// Act runs a page's action and records its outcome in the tab's row: the
// op id, and the message of a refusal. Cmd runs in a savepoint of its
// own, so a refusal undoes its changes and drops its topics, while the
// tab's row is written all the same: the tab's view brings the op id back
// whatever became of it.
type Act struct {
	Session, Tab, Op string
	Cmd              writer.Command
	At               time.Time
}

func (a Act) Validate() error {
	if a.Session == "" || a.Tab == "" || a.Op == "" || a.Cmd == nil {
		return errors.New("an action needs a session, a tab, an op and a command")
	}
	if v, ok := a.Cmd.(writer.Validator); ok {
		return v.Validate()
	}
	return nil
}

func (a Act) Apply(tx *writer.Tx) error {
	state, err := Tab(tx.Context(), tx, a.Session, a.Tab)
	if err != nil {
		return err
	}
	state.Op, state.Refusal = a.Op, ""
	var refusal *writer.Refusal
	switch err := tx.Savepoint(a.Cmd.Apply); {
	case errors.As(err, &refusal):
		state.Refusal = refusal.Msg
	case err != nil:
		return err
	}
	return saveTab(tx, a.Session, a.Tab, state, a.At)
}

// Assign gives a card to someone, and writes the mail that tells them
// into the outbox in the same transaction: the mail exists if and only if
// the assignment committed. Wake, if set, runs after the commit to tell
// the sender (Outbox.Wake).
type Assign struct {
	Card  int64
	Email string
	At    time.Time
	Wake  func()
}

func (c Assign) Validate() error {
	if c.Card <= 0 || !strings.Contains(c.Email, "@") {
		return errors.New("an assignment needs a card and an address")
	}
	return nil
}

func (c Assign) Apply(tx *writer.Tx) error {
	var board int64
	var title, from string
	err := tx.QueryRow(`SELECT l.board_id, c.title, coalesce(c.assignee, '') FROM cards c JOIN lanes l ON l.id = c.lane_id
		WHERE c.id = ? AND c.archived IS NULL`, c.Card).Scan(&board, &title, &from)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE cards SET assignee = ? WHERE id = ?`, c.Email, c.Card); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO outbox (to_addr, subject, at) VALUES (?, ?, ?)`,
		c.Email, "A card for you: "+title, c.At.Unix()); err != nil {
		return err
	}
	tx.Touch(Topic(board), CardTopic(c.Card))
	if c.Wake != nil {
		tx.AfterCommit(c.Wake) // on the writer's goroutine, after the commit: must not block
	}
	tx.Publish(Assigned{Card: c.Card, From: from, To: c.Email}) // for in-process listeners, such as Workload
	return nil
}

// ImportCards inserts cards that arrive with their ids, from another
// system. Import splits a large list into these.
type ImportCards struct{ Cards []Card }

func (c ImportCards) Apply(tx *writer.Tx) error {
	lanes := map[int64]bool{}
	for _, card := range c.Cards {
		if _, err := tx.Exec(`INSERT INTO cards (id, lane_id, title) VALUES (?, ?, ?)`, card.ID, card.Lane, card.Title); err != nil {
			return err
		}
		lanes[card.Lane] = true
	}
	for lane := range lanes {
		var board int64
		if err := tx.QueryRow(`SELECT board_id FROM lanes WHERE id = ?`, lane).Scan(&board); err != nil {
			return err
		}
		tx.Touch(Topic(board))
	}
	return nil
}

// Import inserts cards in commands of 2,000, sorted by id: other commands
// get their turn between two chunks, and sorted keys append to the
// table's B-tree where random ones would land all over it.
func Import(ctx context.Context, w *writer.Writer, cards []Card) error {
	slices.SortFunc(cards, func(a, b Card) int { return cmp.Compare(a.ID, b.ID) })
	for chunk := range slices.Chunk(cards, 2000) {
		if err := w.Do(ctx, ImportCards{Cards: chunk}); err != nil {
			return err
		}
	}
	return nil
}

// ImportFile copies every board of another database file with this
// schema, in one transaction on the writer's own connection. SQLite
// refuses ATTACH inside a transaction, so it runs in Exclusive, between
// two batches, and the commands queued meanwhile wait. Exclusive reaches
// no OnCommit: the caller tells the hub what changed (hub.MarkAll).
func ImportFile(ctx context.Context, w *writer.Writer, path string) error {
	return w.Exclusive(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS src`, path); err != nil {
			return err
		}
		defer conn.ExecContext(context.WithoutCancel(ctx), `DETACH DATABASE src`)
		tx, err := conn.BeginTx(ctx, nil) // BEGIN IMMEDIATE, as every transaction on the write connection
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, q := range []string{
			`INSERT INTO boards SELECT * FROM src.boards`,
			`INSERT INTO lanes SELECT * FROM src.lanes`,
			`INSERT INTO cards SELECT * FROM src.cards`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}

// Sweep keeps the tables that grow bounded: the rows of tabs quiet for
// TabTTL, the preferences of sessions unseen for a year, and the cards
// archived more than a month ago, with their streams and their history.
type Sweep struct{ At time.Time }

func (c Sweep) Apply(tx *writer.Tx) error {
	month := c.At.AddDate(0, -1, 0).Unix()
	if err := forgetCards(tx, month); err != nil {
		return err
	}
	for _, q := range []struct {
		sql string
		arg int64
	}{
		{`DELETE FROM tab_state WHERE seen < ?`, c.At.Add(-TabTTL).Unix()},
		{`DELETE FROM prefs WHERE seen < ?`, c.At.AddDate(-1, 0, 0).Unix()},
		{`DELETE FROM card_history WHERE card_id IN (SELECT id FROM cards WHERE archived < ?)`, month},
		{`DELETE FROM cards WHERE archived < ?`, month},
	} {
		if _, err := tx.Exec(q.sql, q.arg); err != nil {
			return err
		}
	}
	return nil // archived cards show on no board: no topic changed
}

// forgetCards forgets the streams of the cards archived before a time:
// their events and their state rows. No rebuild can bring them back, so
// the sweep deletes the cards' history in the same command.
func forgetCards(tx *writer.Tx, before int64) error {
	rows, err := tx.Query(`SELECT id FROM cards WHERE archived < ?`, before)
	if err != nil {
		return err
	}
	var gone []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		gone = append(gone, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, id := range gone {
		if _, err := events.Forget(Events, tx, Cards.ID(id)); err != nil {
			return err
		}
	}
	return nil
}
