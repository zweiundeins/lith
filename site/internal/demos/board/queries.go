package board

import (
	"context"
	"time"

	"github.com/zweiundeins/lith/sqlite"
)

// readBoard reads the board: the cards lane by lane, top to bottom, and
// the last move.
func readBoard(ctx context.Context, q sqlite.Querier) (Board, error) {
	var b Board
	var movedAt, lastCard int64
	var lastLane int
	if err := q.QueryRowContext(ctx, `SELECT version, moved_at, last_card, last_lane, last_by FROM board_meta WHERE id = 1`).
		Scan(&b.Version, &movedAt, &lastCard, &lastLane, &b.LastBy); err != nil {
		return b, err
	}
	if movedAt > 0 {
		b.MovedAt = time.Unix(movedAt, 0)
	}
	rows, err := q.QueryContext(ctx, `SELECT id, title, tag, lane, pos, version, moved_by, moved_at FROM board_cards ORDER BY lane, pos`)
	if err != nil {
		return b, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Card
		var at int64
		if err := rows.Scan(&c.ID, &c.Title, &c.Tag, &c.Lane, &c.Pos, &c.Version, &c.MovedBy, &at); err != nil {
			return b, err
		}
		if at > 0 {
			c.MovedAt = time.Unix(at, 0)
		}
		if c.Lane < 0 || c.Lane >= Lanes {
			continue
		}
		b.Lanes[c.Lane] = append(b.Lanes[c.Lane], c)
		if c.ID == lastCard {
			b.Last = c
		}
	}
	if b.Last.ID == 0 {
		b.LastBy = ""
	}
	return b, rows.Err()
}

// Person is someone on the board's page: one avatar, however many tabs.
type Person struct {
	Avatar
	Tabs int
}

// present returns who has the board's page open: the sessions of the
// tabs that said hello and whose stream is open now, in the order they
// came. open reports whether a tab's stream is open.
func present(ctx context.Context, q sqlite.Querier, open func(tab string) bool) ([]Person, error) {
	rows, err := q.QueryContext(ctx, `SELECT tab, who FROM board_here ORDER BY since, tab LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var people []Person
	index := map[string]int{}
	for rows.Next() {
		var tab, who string
		if err := rows.Scan(&tab, &who); err != nil {
			return nil, err
		}
		if !open(tab) {
			continue
		}
		if i, ok := index[who]; ok {
			people[i].Tabs++
			continue
		}
		index[who] = len(people)
		people = append(people, Person{Avatar: AvatarOf(who), Tabs: 1})
	}
	return people, rows.Err()
}
