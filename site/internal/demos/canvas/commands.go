package canvas

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// Paint paints cells of the canvas in one colour for a tab: the batch of a
// stroke that sb-pixel-board sends every 80 ms. It renews the day of every
// cell it paints, counts the pixels for the tab and the last minute, and
// makes the colour the tab's.
type Paint struct {
	Viewer demo.Viewer
	Color  int
	Cells  []int // distinct
	At     time.Time
}

func (c Paint) Validate() error {
	switch {
	case c.Viewer.Tab == "":
		return errors.New("a paint needs a tab")
	case c.Color < 0 || c.Color > 15:
		return errors.New("a colour is 0 to 15")
	case len(c.Cells) == 0 || len(c.Cells) > MaxPaint:
		return fmt.Errorf("paint 1 to %d cells at a time", MaxPaint)
	}
	for _, i := range c.Cells {
		if i < 0 || i >= Cells {
			return errors.New("a cell is off the canvas")
		}
	}
	return nil
}

func (c Paint) Apply(tx *writer.Tx) error {
	changed, err := paintCells(tx, c.Cells, c.Color, c.At)
	if err != nil {
		return err
	}
	if changed {
		tx.Touch(TopicCells)
	}
	tx.Touch(TopicPaints)
	id, err := upsertPainter(tx, c.Viewer, c.Color, len(c.Cells), c.At)
	if err != nil {
		return err
	}
	return countRecent(tx, id, len(c.Cells), c.At)
}

// paintCells sets the cells' colour in the board's row and starts their
// day, and reports whether a colour changed.
func paintCells(tx *writer.Tx, list []int, color int, at time.Time) (bool, error) {
	var cells []byte
	if err := tx.QueryRow(`SELECT cells FROM canvas_board WHERE id = 1`).Scan(&cells); err != nil {
		return false, err
	}
	b, changed := board(cells), 0
	for _, i := range list {
		if b.color(i) != color {
			b.setColor(i, color)
			changed = 1
		}
	}
	if _, err := tx.Exec(`UPDATE canvas_board SET version = version + ?, pixels = pixels + ?, cells = ? WHERE id = 1`,
		changed, len(list), cells); err != nil {
		return false, err
	}
	var err error
	if color == 0 { // a blank cell has no day to live
		_, err = tx.Exec(`DELETE FROM canvas_painted WHERE cell IN (SELECT value FROM json_each(?))`, cellList(list))
	} else {
		_, err = tx.Exec(`INSERT INTO canvas_painted (cell, minute) SELECT value, ? FROM json_each(?) WHERE true
			ON CONFLICT (cell) DO UPDATE SET minute = excluded.minute`, minute(at), cellList(list))
	}
	return changed == 1, err
}

// countRecent adds the pixels to the painter's second, and forgets the
// seconds that have left the last minute.
func countRecent(tx *writer.Tx, painter int64, pixels int, at time.Time) error {
	sec := at.Unix()
	if _, err := tx.Exec(`INSERT INTO canvas_recent (sec, painter, pixels) VALUES (?, ?, ?)
		ON CONFLICT (sec, painter) DO UPDATE SET pixels = pixels + excluded.pixels`, sec, painter, pixels); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM canvas_recent WHERE sec <= ?`, sec-Window)
	return err
}

// Pick makes a colour a tab's: the page sends it when the tab opens and
// whenever the painter picks another, so the painters' list shows each
// one in the colour of their brush.
type Pick struct {
	Viewer demo.Viewer
	Color  int
	At     time.Time
}

func (c Pick) Validate() error {
	switch {
	case c.Viewer.Tab == "":
		return errors.New("a pick needs a tab")
	case c.Color < 0 || c.Color > 15:
		return errors.New("a colour is 0 to 15")
	}
	return nil
}

func (c Pick) Apply(tx *writer.Tx) error {
	_, err := upsertPainter(tx, c.Viewer, c.Color, 0, c.At)
	return err
}

// upsertPainter records a tab's colour and pixels, touches the painters'
// topic when the list changes (a new painter, a new colour), and returns
// the painter's id.
func upsertPainter(tx *writer.Tx, v demo.Viewer, color, pixels int, at time.Time) (int64, error) {
	tag := Tag(v.Tab)
	var id int64
	var old int
	err := tx.QueryRow(`SELECT id, color FROM canvas_painters WHERE tag = ?`, tag).Scan(&id, &old)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.Exec(`INSERT INTO canvas_painters (tag, sid, color, pixels, seen) VALUES (?, ?, ?, ?, ?)`,
			tag, v.Session, color, pixels, at.Unix())
		if err != nil {
			return 0, err
		}
		tx.Touch(TopicPainters)
		return res.LastInsertId()
	case err != nil:
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE canvas_painters SET color = ?, pixels = pixels + ?, seen = ? WHERE id = ?`,
		color, pixels, at.Unix(), id); err != nil {
		return 0, err
	}
	if old != color {
		tx.Touch(TopicPainters)
	}
	return id, nil
}

// sweep fades the pixels whose day is over, forgets the seconds that
// have left the last minute, and the painters not seen for a day.
func sweep(tx *writer.Tx, now time.Time) error {
	if err := fade(tx, now); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM canvas_recent WHERE sec <= ?`, now.Unix()-Window)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		tx.Touch(TopicPaints, TopicPainters)
	}
	n, err := demo.DeleteOlder(tx, "canvas_painters", "seen", now.Add(-Lifetime))
	if n > 0 {
		tx.Touch(TopicPainters)
	}
	return err
}

// fade paints the cells whose day is over in the background, and touches
// the board's topics if there were any.
func fade(tx *writer.Tx, now time.Time) error {
	limit := minute(now.Add(-Lifetime))
	rows, err := tx.Query(`SELECT cell FROM canvas_painted WHERE minute <= ?`, limit)
	if err != nil {
		return err
	}
	var faded []int
	for rows.Next() {
		var cell int
		if err := rows.Scan(&cell); err != nil {
			rows.Close()
			return err
		}
		faded = append(faded, cell)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil || len(faded) == 0 {
		return err
	}
	var cells []byte
	if err := tx.QueryRow(`SELECT cells FROM canvas_board WHERE id = 1`).Scan(&cells); err != nil {
		return err
	}
	for _, i := range faded {
		board(cells).setColor(i, 0)
	}
	if _, err := tx.Exec(`UPDATE canvas_board SET version = version + 1, cells = ? WHERE id = 1`, cells); err != nil {
		return err
	}
	_, err = tx.Exec(`DELETE FROM canvas_painted WHERE minute <= ?`, limit)
	tx.Touch(TopicCells, TopicPaints)
	return err
}
