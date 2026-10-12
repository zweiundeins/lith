package guestbook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Latest returns the newest n entries, newest first, and how many there are.
func Latest(ctx context.Context, tx *sql.Tx, n int) ([]Entry, int, error) {
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM entries`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, name, message, at FROM entries ORDER BY id DESC LIMIT ?`, n)
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

// Tab returns the state of a tab. A tab that never signed has no row yet.
func Tab(ctx context.Context, tx *sql.Tx, sid, tab string) (TabState, error) {
	var data string
	err := tx.QueryRowContext(ctx, `SELECT data FROM tab_state WHERE sid = ? AND tab_id = ?`, sid, tab).Scan(&data)
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
