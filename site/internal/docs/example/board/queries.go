package board

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zweiundeins/lith/sqlite"
)

// LaneCount is the number of cards on a lane. It takes a sqlite.Querier,
// so a GET, a view and a command all call it: a *sql.Tx, a render's
// sqlite.QuerierFrom(ctx) and a *writer.Tx are each one.
func LaneCount(ctx context.Context, q sqlite.Querier, lane int64) (n int, err error) {
	err = q.QueryRowContext(ctx, `SELECT count(*) FROM cards WHERE lane_id = ? AND archived IS NULL`, lane).Scan(&n)
	return n, err
}

// Load reads a board with its lanes and their cards. Called with one
// transaction, its three queries see one snapshot.
func Load(ctx context.Context, q sqlite.Querier, id int64) (Board, error) {
	b := Board{ID: id}
	err := q.QueryRowContext(ctx, `SELECT name FROM boards WHERE id = ?`, id).Scan(&b.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	if err != nil {
		return b, err
	}
	rows, err := q.QueryContext(ctx, `SELECT id, name, max_cards FROM lanes WHERE board_id = ? ORDER BY id`, id)
	if err != nil {
		return b, err
	}
	defer rows.Close()
	at := map[int64]int{} // lane id to its index
	for rows.Next() {
		var l Lane
		if err := rows.Scan(&l.ID, &l.Name, &l.Max); err != nil {
			return b, err
		}
		at[l.ID] = len(b.Lanes)
		b.Lanes = append(b.Lanes, l)
	}
	if err := rows.Err(); err != nil {
		return b, err
	}
	cards, err := q.QueryContext(ctx, `SELECT c.id, c.lane_id, c.title, c.version FROM cards c
		JOIN lanes l ON l.id = c.lane_id WHERE l.board_id = ? AND c.archived IS NULL ORDER BY c.id`, id)
	if err != nil {
		return b, err
	}
	defer cards.Close()
	for cards.Next() {
		var c Card
		if err := cards.Scan(&c.ID, &c.Lane, &c.Title, &c.Version); err != nil {
			return b, err
		}
		l := &b.Lanes[at[c.Lane]]
		l.Cards = append(l.Cards, c)
	}
	return b, cards.Err()
}

// LoadCard reads one card, for its panel.
func LoadCard(ctx context.Context, q sqlite.Querier, id int64) (Card, error) {
	c := Card{ID: id}
	err := q.QueryRowContext(ctx, `SELECT lane_id, title, version FROM cards WHERE id = ?`, id).Scan(&c.Lane, &c.Title, &c.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}
