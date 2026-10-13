package checkboxes

import (
	"errors"
	"fmt"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// Hub is what the demo asks of lith's live hub: to change what a tab's
// stream watches, and how many streams watch a view.
type Hub interface {
	Watch(streamID string, v live.View) bool
	Unwatch(streamID, key string) bool
	Count(key string) int
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
	tx.Touch(ChunkTopic(n), StatsTopic)
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

// Rewatch makes a tab's stream watch the chunks of the window the tab
// stored. A stream opens with the tab's own view and the numbers only, so
// a page that comes back, after a deploy or a dropped connection, gets its
// chunks from here, in the writer's order like SetWindow's.
type Rewatch struct {
	Viewer demo.Viewer
	Hub    Hub
}

func (c Rewatch) Apply(tx *writer.Tx) error {
	w, err := window(tx.Context(), tx, c.Viewer)
	if err != nil {
		return err
	}
	tx.AfterCommit(func() { watch(c.Hub, c.Viewer.Tab, w) })
	return nil
}

// follow moves a tab's stream from one window to the next.
func follow(h Hub, tab string, from, to Window) {
	if h == nil {
		return
	}
	for n := from.Start; n < from.End(); n++ {
		if !to.Has(n) {
			h.Unwatch(tab, ChunkTopic(n))
		}
	}
	watch(h, tab, to)
}

// watch makes a tab's stream watch the window's chunks. A chunk it
// watches already stays as it is; a new one comes with its newest frame.
func watch(h Hub, tab string, w Window) {
	if h == nil {
		return
	}
	for n := w.Start; n < w.End(); n++ {
		h.Watch(tab, chunkView(h, n))
	}
}

// sweep keeps the per-second counts of the last hour. The chunks and the
// totals never grow, and the site sweeps the tabs' windows.
func sweep(tx *writer.Tx, now time.Time) error {
	_, err := demo.DeleteOlder(tx, "checkboxes_seconds", "sec", now.Add(-KeptSeconds))
	return err
}
