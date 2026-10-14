package life

import (
	"context"
	"io"
	"strconv"
	"time"

	"github.com/a-h/templ"
)

// cellClass is the class of each colour's cells, a letter: _ for a dead
// cell, a to h for the people's colours, w for the soup's. A cell that
// died a generation ago takes its colour's capital, two generations ago
// the letter in trailClass.
const (
	cellClass  = "_abcdefghw"
	dyingClass = "_ABCDEFGHW"
	trailClass = "_ijklmnopx"
)

// cellTags are each colour's cell: a <p> with a class of one letter.
// A <p> closes the one before it, so a cell needs no end tag, and every
// cell has the same length: a frame then lines up byte for byte with the
// one before it, and zstd sends little more than the cells that changed.
// With dead cells shorter than live ones, each birth or death shifts the
// rest of the frame, and a frame takes almost twice the bytes.
var cellTags = func() (t [3][len(cellClass)]string) {
	for age, class := range [...]string{cellClass, dyingClass, trailClass} {
		for i := range class {
			t[age][i] = "<p class=" + class[i:i+1] + ">"
		}
	}
	return t
}()

// rowTags open each row of the board. A row's id lets Datastar's morph
// pair it with the row it replaces instead of walking 4,000 cells: a
// frame then costs the browser less than half the script time and a fifth
// of the style work.
var rowTags = func() (t [H]string) {
	for y := range t {
		t[y] = `<div id="life-r` + strconv.Itoa(y) + `">`
	}
	return t
}()

// boardCells is the board's element: H rows of W cells, which CSS lays
// out in one grid. Screen readers skip it; the counters say what it holds.
func boardCells(cells, trail []byte) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		const open, end = `<div id="life-board" class="life__board" aria-hidden="true">`, `</div>`
		b := make([]byte, 0, len(open)+H*(len(rowTags[H-1])+len("</div>"))+len(cells)*len(cellTags[0][0])+len(end))
		b = append(b, open...)
		for y, row := range rowTags {
			b = append(b, row...)
			for x, c := range cells[y*W : (y+1)*W] {
				if t := trailAt(trail, y*W+x); c == 0 && t != 0 {
					b = append(b, cellTags[t>>4][t&15]...)
					continue
				}
				b = append(b, cellTags[0][c]...)
			}
			b = append(b, "</div>"...)
		}
		_, err := w.Write(append(b, end...))
		return err
	})
}

// trailAt is the trail at cell i, 0 where there is none.
func trailAt(trail []byte, i int) byte {
	if len(trail) != Cells {
		return 0
	}
	return trail[i]
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
