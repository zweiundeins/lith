package cursorparty

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/zweiundeins/lith/sqlite"
)

// Cursor is a tab's pointer at the party.
type Cursor struct {
	ID   string
	Name string
	Hue  int
	X, Y float64
	At   time.Time
}

// cursors returns the cursors that moved within Gone before now, at most
// Shown of them: the newest, those whose tabs arrived last. A crowd of
// more than Shown keeps the same cursors from one frame to the next, where
// the ones that moved last would change with every frame. They come in the
// order of their ids: a cursor keeps its place in the layer, so a morph
// never moves an element, which would restart its transitions.
func cursors(ctx context.Context, q sqlite.Querier, now time.Time) ([]Cursor, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, name, hue, x, y, at FROM cursor_party_cursors WHERE at >= ? ORDER BY born DESC LIMIT ?`,
		now.Add(-Gone).UnixMilli(), Shown)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Cursor
	for rows.Next() {
		var c Cursor
		var at int64
		if err := rows.Scan(&c.ID, &c.Name, &c.Hue, &c.X, &c.Y, &at); err != nil {
			return nil, err
		}
		c.At = time.UnixMilli(at)
		list = append(list, c)
	}
	slices.SortFunc(list, func(a, b Cursor) int { return strings.Compare(a.ID, b.ID) })
	return list, rows.Err()
}

// movesPerSecond is how many moves the writer applied in the last whole
// second before now.
func movesPerSecond(ctx context.Context, q sqlite.Querier, now time.Time) (int, error) {
	var second, moves, last int64
	if err := q.QueryRowContext(ctx, `SELECT second, moves, last FROM cursor_party_rate WHERE id = 1`).Scan(&second, &moves, &last); err != nil {
		return 0, err
	}
	switch now.Unix() {
	case second + 1:
		return int(moves), nil
	case second:
		return int(last), nil
	}
	return 0, nil
}
