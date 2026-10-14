package cursorparty

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// Move puts the viewer's cursor where its pointer is: A names the anchor
// nearest to it (Anchors), and X and Y are fractions of that anchor's
// width and height, beyond it on the side the pointer is. Seq is the page's count of
// its moves: requests can overtake each other, and a move older than the
// one stored changes nothing.
type Move struct {
	Viewer demo.Viewer
	A      string
	X, Y   float64
	Seq    int64
	At     time.Time
}

// maxFraction bounds a place beside its anchor: the far corners of a wide
// floor are a few of an anchor's widths away.
const maxFraction = 20

// maxSeq is the largest integer a page counts to exactly (2^53).
const maxSeq = 1 << 53

func (c Move) Validate() error {
	switch {
	case !demo.ValidID(c.Viewer.Tab) || c.Viewer.Session == "":
		return errors.New("a move needs a tab and a session")
	case !knownAnchor(c.A):
		return errors.New("a move names one of the party's anchors")
	case (c.A == "" || c.A == floorAnchor) && !(c.X >= 0 && c.X <= 1 && c.Y >= 0 && c.Y <= 1):
		return errors.New("a position on the floor is a fraction of its size, from 0 to 1")
	case !(c.X >= -maxFraction && c.X <= maxFraction && c.Y >= -maxFraction && c.Y <= maxFraction):
		return errors.New("a position is a fraction of its anchor's size, from -20 to 20")
	case c.Seq < 1 || c.Seq > maxSeq:
		return errors.New("a move's number counts from 1")
	}
	return nil
}

func (c Move) Apply(tx *writer.Tx) error {
	id, name, hue := Identity(c.Viewer.Tab)
	at := c.At.UnixMilli()
	if c.A == "" {
		c.A = floorAnchor
	}
	res, err := tx.Exec(`UPDATE cursor_party_cursors SET a = ?, x = ?, y = ?, seq = ?, at = ? WHERE id = ? AND sid = ? AND seq < ?`,
		c.A, c.X, c.Y, c.Seq, at, id, c.Viewer.Session, c.Seq)
	if err != nil {
		return err
	}
	moved, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if moved == 0 {
		// The tab's first move, or one that a newer move overtook, or a tab
		// id from another session: only the first inserts a row.
		res, err := tx.Exec(`INSERT INTO cursor_party_cursors (id, sid, name, hue, a, x, y, seq, born, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO NOTHING`, id, c.Viewer.Session, name, hue, c.A, c.X, c.Y, c.Seq, at, at)
		if err != nil {
			return err
		}
		if moved, err = res.RowsAffected(); err != nil || moved == 0 {
			return err
		}
		if err := makeRoom(tx, at); err != nil {
			return err
		}
	}
	// The counter's moves of each second.
	if _, err := tx.Exec(`INSERT INTO cursor_party_seconds (second, moves) VALUES (?, 1)
		ON CONFLICT (second) DO UPDATE SET moves = moves + 1`, c.At.Unix()); err != nil {
		return err
	}
	tx.Touch(Topic)
	return nil
}

// makeRoom runs when a new cursor arrives: the rows of tabs quiet for
// Keep go, and the oldest beyond MaxCursors, so the table stays small
// between two sweeps whatever tab ids the requests make up.
func makeRoom(tx *writer.Tx, at int64) error {
	if _, err := tx.Exec(`DELETE FROM cursor_party_cursors WHERE at < ?`, at-Keep.Milliseconds()); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM cursor_party_cursors WHERE at < (SELECT at FROM cursor_party_cursors ORDER BY at DESC LIMIT 1 OFFSET ?)`, MaxCursors-1)
	return err
}

// sweep deletes the rows of tabs that have not moved for Keep, and the
// seconds older than that.
func sweep(tx *writer.Tx, now time.Time) error {
	if _, err := tx.Exec(`DELETE FROM cursor_party_seconds WHERE second < ?`, now.Add(-Keep).Unix()); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM cursor_party_cursors WHERE at < ?`, now.Add(-Keep).UnixMilli())
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return err
	}
	tx.Touch(Topic)
	return nil
}

// Identity is what the party knows of a tab: its cursor's id, 12 hex
// digits of a SHA-256 of the tab id, and the name and the colour that
// follow from it. The id is public, the tab id stays secret: a stream
// opened with another tab's id would replace that tab's own.
func Identity(tab string) (id, name string, hue int) {
	h := sha256.Sum256([]byte(Slug + ":" + tab))
	hue = int(h[6]) % len(hues)
	return hex.EncodeToString(h[:6]), hues[hue] + " " + animals[int(h[7])%len(animals)], hue
}

// hues name the party's colours, in the order of --_h0 to --_h5 in
// cursor-party.css: names that say the colour, for those who cannot tell
// two of them apart.
var hues = []string{"ember", "gold", "sage", "moss", "rust", "ash"}

var animals = []string{
	"fox", "owl", "moth", "wren", "hare", "lynx", "seal", "crow",
	"newt", "toad", "mole", "vole", "kite", "yak", "elk", "ibex",
	"crab", "carp", "pike", "wolf", "bear", "lark", "swan", "duck",
	"goat", "mink", "otter", "heron", "finch", "gecko", "koala", "panda",
}
