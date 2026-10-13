package canvas

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/zweiundeins/lith/sqlite"
)

// Painter is a tab on the canvas's page, as the list of painters shows
// it.
type Painter struct {
	Tag    string
	Color  int  // -1 until the tab's first pick arrives
	Pixels int  // painted in the last minute
	Active bool // painted in the last few seconds
	id     int64
}

// activeFor is how recent a painter's last paint is while they count as
// painting.
const activeFor = 4 // seconds

// painters returns the painters of the tabs: those that picked a colour
// in the order they came, then the others. Every query is a point lookup
// or a scan of the last minute, so a render with 300 tabs stays short.
func painters(ctx context.Context, q sqlite.Querier, tabs []string, now time.Time) ([]Painter, error) {
	type activity struct {
		pixels int
		last   int64
	}
	recent := map[int64]activity{}
	rows, err := q.QueryContext(ctx, `SELECT painter, sum(pixels), max(sec) FROM canvas_recent WHERE sec > ? GROUP BY painter`, now.Unix()-Window)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var a activity
		if err := rows.Scan(&id, &a.pixels, &a.last); err != nil {
			return nil, err
		}
		recent[id] = a
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var known, unknown []Painter
	for _, tab := range tabs {
		p := Painter{Tag: Tag(tab), Color: -1}
		err := q.QueryRowContext(ctx, `SELECT id, color FROM canvas_painters WHERE tag = ?`, p.Tag).Scan(&p.id, &p.Color)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			unknown = append(unknown, p)
		case err != nil:
			return nil, err
		default:
			a := recent[p.id]
			p.Pixels, p.Active = a.pixels, a.last > now.Unix()-activeFor
			known = append(known, p)
		}
	}
	slices.SortFunc(known, func(a, b Painter) int { return cmp.Compare(a.id, b.id) })
	return append(known, unknown...), nil
}

// Pulse is what the canvas has done lately.
type Pulse struct {
	Minute  int         // pixels painted in the last minute
	Seconds [Window]int // the same per second, the oldest first
	Peak    int         // the busiest of those seconds
	On      int         // cells painted now
	Fading  int         // of those, the cells whose day ends within the hour
	Total   int64       // pixels painted since the start
}

func pulse(ctx context.Context, q sqlite.Querier, now time.Time) (Pulse, error) {
	var p Pulse
	if err := q.QueryRowContext(ctx, `SELECT pixels FROM canvas_board WHERE id = 1`).Scan(&p.Total); err != nil {
		return p, err
	}
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM canvas_painted`).Scan(&p.On); err != nil {
		return p, err
	}
	// The cells whose day ends within the hour: painted a day before the
	// next hour's end, or earlier.
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM canvas_painted WHERE minute <= ?`, minute(now.Add(time.Hour-Lifetime))).Scan(&p.Fading); err != nil {
		return p, err
	}
	rows, err := q.QueryContext(ctx, `SELECT sec, sum(pixels) FROM canvas_recent WHERE sec > ? GROUP BY sec`, now.Unix()-Window)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var sec int64
		var n int
		if err := rows.Scan(&sec, &n); err != nil {
			return p, err
		}
		if i := Window - 1 - int(now.Unix()-sec); i >= 0 && i < Window {
			p.Seconds[i] = n
			p.Minute += n
			p.Peak = max(p.Peak, n)
		}
	}
	return p, rows.Err()
}

// lastColor is the colour the session painted with last, for the brush of
// its next tab.
func lastColor(ctx context.Context, q sqlite.Querier, sid string) (int, error) {
	var c int
	err := q.QueryRowContext(ctx, `SELECT color FROM canvas_painters WHERE sid = ? ORDER BY seen DESC LIMIT 1`, sid).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultColor, nil
	}
	return c, err
}
