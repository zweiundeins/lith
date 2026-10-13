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

// History is how many whole seconds of moves the counter's sparkline
// shows.
const History = 30

// history returns the moves of each of the History whole seconds before
// now, the oldest first. The last is the counter's moves a second; the
// second under way is left out, so the numbers change once a second.
func history(ctx context.Context, q sqlite.Querier, now time.Time) ([]int, error) {
	from := now.Unix() - History
	rows, err := q.QueryContext(ctx, `SELECT second, moves FROM cursor_party_seconds WHERE second >= ? AND second < ?`, from, from+History)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	h := make([]int, History)
	for rows.Next() {
		var second int64
		var n int
		if err := rows.Scan(&second, &n); err != nil {
			return nil, err
		}
		h[second-from] = n
	}
	return h, rows.Err()
}
