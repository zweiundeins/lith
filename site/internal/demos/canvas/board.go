package canvas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
)

// board is the canvas's colours as canvas_board stores them: two palette
// indices a byte, the first in the high nibble.
type board []byte

func (b board) color(i int) int {
	if i&1 == 0 {
		return int(b[i>>1] >> 4)
	}
	return int(b[i>>1] & 0x0f)
}

func (b board) setColor(i, c int) {
	v := &b[i>>1]
	if i&1 == 0 {
		*v = *v&0x0f | byte(c)<<4
	} else {
		*v = *v&0xf0 | byte(c)
	}
}

// minute is t in unix minutes, as canvas_painted stores it.
func minute(t time.Time) int64 { return t.Unix() / 60 }

// cellList is cells as a JSON array, for json_each: one statement writes
// a paint's cells, however many.
func cellList(cells []int) string {
	b := make([]byte, 0, 2+5*len(cells))
	b = append(b, '[')
	for i, c := range cells {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendInt(b, int64(c), 10)
	}
	return string(append(b, ']'))
}

// instance is what the process keeps of one database's canvas: the hub
// that serves its pages, and the board encoded as the cells attribute,
// for one version. The site serves one canvas; its tests run several,
// which the board's epoch tells apart.
type instance struct {
	hub     atomic.Pointer[live.Hub]
	encoded atomic.Pointer[encoded]
}

type encoded struct {
	version int64
	cells   string
}

var instances sync.Map // the board's epoch: *instance

func instanceOf(epoch int64) *instance {
	if in, ok := instances.Load(epoch); ok {
		return in.(*instance)
	}
	in, _ := instances.LoadOrStore(epoch, &instance{})
	return in.(*instance)
}

// watchers are the tab ids of the streams on the view: nil where no hub
// serves the canvas, as in a test of the views.
func (in *instance) watchers(key string) []string {
	if h := in.hub.Load(); h != nil {
		return h.Watchers(key)
	}
	return nil
}

// cellsOf reads the board as its cells attribute, one hex digit per cell:
// the BLOB in hex. Only the first read of a version encodes it; the
// renders and documents after it take the cached string.
func cellsOf(ctx context.Context, q sqlite.Querier) (string, *instance, error) {
	var epoch, version int64
	if err := q.QueryRowContext(ctx, `SELECT epoch, version FROM canvas_board WHERE id = 1`).Scan(&epoch, &version); err != nil {
		return "", nil, err
	}
	in := instanceOf(epoch)
	if e := in.encoded.Load(); e != nil && e.version == version {
		return e.cells, in, nil
	}
	var cells []byte
	if err := q.QueryRowContext(ctx, `SELECT cells FROM canvas_board WHERE id = 1`).Scan(&cells); err != nil {
		return "", nil, err
	}
	e := &encoded{version: version, cells: hex.EncodeToString(cells)}
	for {
		old := in.encoded.Load()
		if old != nil && old.version >= version || in.encoded.CompareAndSwap(old, e) {
			break // a newer version stays
		}
	}
	return e.cells, in, nil
}

// Tag stands for a tab in the views everyone shares. The tab id itself
// stays with its tab: a stream opened with it would end the tab's own.
func Tag(tab string) string {
	sum := sha256.Sum256([]byte("lith canvas painter\x00" + tab))
	return hex.EncodeToString(sum[:5])
}

// face draws a painter's 5 by 5 pixel face from its tag, mirrored like a
// face: an SVG path in a 5 by 5 box, one rectangle per run of a row.
func face(tag string) string {
	b, err := hex.DecodeString(tag)
	if err != nil || len(b) < 3 {
		return "M2 0h1v5h-1z"
	}
	bits := uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
	if bits&0x7fff == 0 || bits&0x4924 == 0 { // nothing, or nothing down the middle
		bits |= 0x4924
	}
	var p strings.Builder
	for y := range 5 {
		var row [5]bool
		for x := range 3 {
			on := bits>>(y*3+x)&1 == 1
			row[x], row[4-x] = on, on
		}
		for x := 0; x < 5; {
			if !row[x] {
				x++
				continue
			}
			start := x
			for x < 5 && row[x] {
				x++
			}
			fmt.Fprintf(&p, "M%d %dh%dv1h-%dz", start, y, x-start, x-start)
		}
	}
	return p.String()
}

// The palette, by index. Each colour is a token of the site's, named
// --canvas-c<index> in canvas.css: the neutrals turn over with the theme,
// like ink on paper and chalk on slate, and the hues keep their colour,
// the palest three a step darker on paper. The page resolves the tokens to
// the hex colours sb-pixel-board paints with.
var colorNames = [16]string{
	"Background", "Ink", "Ash", "Stone", "Slate", "Shade", "Glow", "Seam",
	"Edge", "Rust", "Flame", "Gold", "Sage", "Green", "Coral", "Wine",
}

// swatchOrder lays the palette out in ramps: the neutrals from the
// background to ink, the embers, then the other hues.
var swatchOrder = [16]int{0, 5, 4, 3, 2, 1, 9, 8, 7, 10, 6, 11, 12, 13, 14, 15}
