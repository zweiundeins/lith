package life

import (
	"context"
	"io"
	"strconv"
	"time"

	"github.com/a-h/templ"
)

// cellClass is the class of each colour's cells, a letter: _ for a dead
// cell, a to h for the people's colours, w for the soup's.
const cellClass = "_abcdefghw"

// cellTags are each colour's cell: a <p> with a class of one letter.
// A <p> closes the one before it, so a cell needs no end tag, and every
// cell has the same length: a frame then lines up byte for byte with the
// one before it, and zstd sends little more than the cells that changed.
// With dead cells shorter than live ones, each birth or death shifts the
// rest of the frame, and a frame takes almost twice the bytes.
var cellTags = func() (t [len(cellClass)]string) {
	for i := range t {
		t[i] = "<p class=" + cellClass[i:i+1] + ">"
	}
	return t
}()

// boardCells is the board's element: Cells cells in rows of W, which CSS
// lays out in a grid. Screen readers skip it; the counters say what it
// holds.
func boardCells(cells []byte) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		const open, end = `<div id="life-board" class="life__board" aria-hidden="true">`, `</div>`
		b := make([]byte, 0, len(open)+len(cells)*len(cellTags[0])+len(end))
		b = append(b, open...)
		for _, c := range cells {
			b = append(b, cellTags[c]...)
		}
		_, err := w.Write(append(b, end...))
		return err
	})
}

// number writes n with a comma between thousands: 12,345.
func number(n int64) string {
	s := strconv.FormatInt(n, 10)
	if n < 0 {
		return "-" + number(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// tenths writes a number of tenths with one decimal: 10.0.
func tenths(n int) string { return strconv.Itoa(n/10) + "." + strconv.Itoa(n%10) }

// status says what the board does now, and when a quiet one gets a new
// soup.
func status(b Board, now time.Time) string {
	if b.Quiet == 0 {
		return "Draw on it, or drop a pattern: everyone here sees it in the next generation."
	}
	left := time.Duration(b.Quiet)*time.Millisecond + b.quietFor() - time.Duration(now.UnixMilli())*time.Millisecond
	if Population(b.Cells) == 0 {
		return "The board is empty. A new soup in " + seconds(max(left, 0)) + ", unless someone draws."
	}
	return "The board repeats itself. A new soup in " + seconds(max(left, 0)) + ", unless someone draws."
}

// colourNames name the colours for screen readers, the soup's last.
var colourNames = [Colours + 2]string{"", "Red", "Ember", "Gold", "Green", "Teal", "Blue", "Violet", "Pink", "Soup"}

// share is a colour's part of the live cells, as a flex-grow.
func share(n int) string { return "flex-grow: " + strconv.Itoa(n) }

func letter(c int) string { return cellClass[c : c+1] }
