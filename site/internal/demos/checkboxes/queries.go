package checkboxes

import (
	"context"
	"time"

	"github.com/zweiundeins/lith/sqlite"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// chunk returns the bits of chunk n.
func chunk(ctx context.Context, q sqlite.Querier, n int) ([]byte, error) {
	var bits []byte
	err := q.QueryRowContext(ctx, `SELECT bits FROM checkboxes_chunks WHERE id = ?`, n).Scan(&bits)
	return bits, err
}

// chunks returns the bits of the window's chunks, in order.
func chunks(ctx context.Context, q sqlite.Querier, w Window) ([][]byte, error) {
	rows, err := q.QueryContext(ctx, `SELECT bits FROM checkboxes_chunks WHERE id >= ? AND id < ? ORDER BY id`, w.Start, w.End())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all [][]byte
	for rows.Next() {
		var bits []byte
		if err := rows.Scan(&bits); err != nil {
			return nil, err
		}
		all = append(all, bits)
	}
	return all, rows.Err()
}

// Stats are the numbers above the grid.
type Stats struct {
	Checked, Flips int
	// Rate holds the flips of each of the Trend seconds before now, the
	// last one last.
	Rate []int
	// Here is how many tabs show the demo.
	Here int
}

// PerSecond is the flips of the last whole second.
func (s Stats) PerSecond() int {
	if len(s.Rate) == 0 {
		return 0
	}
	return s.Rate[len(s.Rate)-1]
}

// Peak is the top of the rate's chart: its highest second, and at least
// 10, so that a quiet minute draws a line along the bottom.
func (s Stats) Peak() int {
	peak := 10
	for _, n := range s.Rate {
		peak = max(peak, n)
	}
	return peak
}

// stats reads the totals and the flips of the Trend whole seconds before
// now.
func stats(ctx context.Context, q sqlite.Querier, now time.Time) (Stats, error) {
	var s Stats
	if err := q.QueryRowContext(ctx, `SELECT checked, flips FROM checkboxes_totals WHERE id = 1`).Scan(&s.Checked, &s.Flips); err != nil {
		return s, err
	}
	from := now.Unix() - Trend
	rows, err := q.QueryContext(ctx, `SELECT sec, flips FROM checkboxes_seconds WHERE sec >= ? AND sec < ? ORDER BY sec`, from, now.Unix())
	if err != nil {
		return s, err
	}
	defer rows.Close()
	s.Rate = make([]int, Trend)
	for rows.Next() {
		var sec int64
		var n int
		if err := rows.Scan(&sec, &n); err != nil {
			return s, err
		}
		s.Rate[sec-from] = n
	}
	return s, rows.Err()
}

// window reads the window the tab stored; a tab that stored none shows
// the top of the grid.
func window(ctx context.Context, q sqlite.Querier, v demo.Viewer) (Window, error) {
	var w Window
	err := demo.LoadTab(ctx, q, Slug, v, &w)
	if w.Start < 0 || w.Start >= Chunks {
		w = Window{}
	}
	return w, err
}
