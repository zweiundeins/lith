package checkboxes

import (
	"errors"
	"fmt"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// Hub is what SetWindow asks of lith's live hub: to change what a tab's
// stream watches. The handler hands it the kit's.
type Hub interface {
	Watch(streamID string, v live.View) bool
	Unwatch(streamID, key string) bool
}

var _ Hub = (*live.Hub)(nil)

// SetBox checks or clears one box. A box that is so already stays as it
// is; otherwise its bit flips, the totals follow, and the flip counts in
// its second.
type SetBox struct {
	Box     int // from 0
	Checked bool
	At      time.Time
}

func (c SetBox) Validate() error {
	if c.Box < 0 || c.Box >= Boxes {
		return fmt.Errorf("a box has a number from 1 to %s", grouped(Boxes))
	}
	return nil
}

func (c SetBox) Apply(tx *writer.Tx) error {
	n, i := c.Box/ChunkSize, c.Box%ChunkSize
	var bits []byte
	if err := tx.QueryRow(`SELECT bits FROM checkboxes_chunks WHERE id = ?`, n).Scan(&bits); err != nil {
		return err
	}
	if isSet(bits, i) == c.Checked {
		return nil // so already: nothing changes, nothing renders
	}
	bits[i>>3] ^= 1 << (i & 7)
	d := -1
	if c.Checked {
		d = 1
	}
	if _, err := tx.Exec(`UPDATE checkboxes_chunks SET bits = ? WHERE id = ?`, bits, n); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE checkboxes_totals SET checked = checked + ?, flips = flips + 1 WHERE id = 1`, d); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO checkboxes_seconds (sec, flips) VALUES (?, 1)
		ON CONFLICT (sec) DO UPDATE SET flips = flips + 1`, c.At.Unix()); err != nil {
		return err
	}
	tx.Touch(ChunkTopic(n))
	return nil
}

// SetWindow stores the window a tab shows, and moves the tab's stream to
// its chunks: Unwatch for the chunks the tab scrolled away from, Watch for
// the others. The watches change after the commit, on the writer's
// goroutine, so they follow the windows in the order the writer stored
// them: the stream watches the chunks of the last one.
type SetWindow struct {
	Viewer demo.Viewer
	Window Window
	At     time.Time
	Hub    Hub
}

func (c SetWindow) Validate() error {
	if c.Viewer.Tab == "" || c.Window.Start < 0 || c.Window.Start >= Chunks {
		return errors.New("a window needs a tab and a chunk")
	}
	return nil
}

func (c SetWindow) Apply(tx *writer.Tx) error {
	old, err := window(tx.Context(), tx, c.Viewer)
	if err != nil {
		return err
	}
	if c.Window != old {
		if err := demo.SaveTab(tx, Slug, c.Viewer, c.Window, c.At); err != nil {
			return err
		}
	}
	tx.AfterCommit(func() { follow(c.Hub, c.Viewer.Tab, old, c.Window) })
	return nil
}

// follow moves a tab's stream from one window to the next: Unwatch for
// the chunks it left, Watch for the window's. A chunk the stream watches
// already stays as it is; a new one comes with its newest frame.
func follow(h Hub, tab string, from, to Window) {
	if h == nil {
		return
	}
	for n := from.Start; n < from.End(); n++ {
		if !to.Has(n) {
			h.Unwatch(tab, ChunkTopic(n))
		}
	}
	for n := to.Start; n < to.End(); n++ {
		h.Watch(tab, chunkView(n))
	}
}

// sweep keeps the per-second counts of the last hour. The chunks and the
// totals never grow, and the site sweeps the tabs' windows.
func sweep(tx *writer.Tx, now time.Time) error {
	_, err := demo.DeleteOlder(tx, "checkboxes_seconds", "sec", now.Add(-KeptSeconds))
	return err
}
