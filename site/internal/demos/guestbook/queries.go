package guestbook

import (
	"context"
	"time"

	"github.com/zweiundeins/lith/sqlite"
)

// latest returns the newest n entries, newest first, and how many there
// are.
func latest(ctx context.Context, q sqlite.Querier, n int) ([]Entry, int, error) {
	var total int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM guestbook_entries`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := q.QueryContext(ctx, `SELECT id, name, message, at FROM guestbook_entries ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var list []Entry
	for rows.Next() {
		var e Entry
		var at int64
		if err := rows.Scan(&e.ID, &e.Name, &e.Message, &at); err != nil {
			return nil, 0, err
		}
		e.At = time.Unix(at, 0)
		list = append(list, e)
	}
	return list, total, rows.Err()
}
