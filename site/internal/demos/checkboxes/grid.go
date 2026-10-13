package checkboxes

import "strconv"

// Window is the chunks a tab watches: Slots chunks from Start, fewer where
// the grid ends. Its tiles are the items of the page's list, which starts
// at Start (the list's offset).
type Window struct {
	Start int `json:"start"`
}

// End is one past the window's last chunk.
func (w Window) End() int { return min(w.Start+Slots, Chunks) }

// Has reports whether chunk n is in the window.
func (w Window) Has(n int) bool { return n >= w.Start && n < w.End() }

// windowFor is the window for what the list asks: count tiles from offset,
// cols to a row. It holds Slots chunks in whole rows, centred on the rows
// asked for, and stays inside the grid.
func windowFor(offset, count, cols int) Window {
	cols = min(max(cols, 1), MaxCols)
	rows := Slots / cols
	asked := (max(count, 1) + cols - 1) / cols
	row := offset/cols - (rows-asked)/2
	last := max((Chunks+cols-1)/cols-rows, 0)
	return Window{Start: min(max(row, 0), last) * cols}
}

// slot is the slot that holds chunk n's tile on every page.
func slot(n int) int { return n % Slots }

// parked are the slots the window leaves without a chunk, where the grid
// ends.
func parked(w Window) []int {
	var out []int
	for n := w.End(); n < w.Start+Slots; n++ {
		out = append(out, slot(n))
	}
	return out
}

// rateJSON is the per-second counts as a JSON array.
func rateJSON(rate []int) string {
	b := []byte{'['}
	for i, n := range rate {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendInt(b, int64(n), 10)
	}
	return string(append(b, ']'))
}

// size is how many boxes chunk n holds.
func size(n int) int { return min(ChunkSize, Boxes-n*ChunkSize) }

// isSet reports whether box i of a chunk is checked.
func isSet(bits []byte, i int) bool { return bits[i>>3]>>(i&7)&1 == 1 }

// grouped writes n with a comma between each group of three digits.
func grouped(n int) string { return string(appendGrouped(nil, n)) }

// appendGrouped appends n to b as grouped writes it.
func appendGrouped(b []byte, n int) []byte {
	if n < 0 {
		b = append(b, '-')
		n = -n
	}
	var digits [20]byte
	s := strconv.AppendInt(digits[:0], int64(n), 10)
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b = append(b, ',')
		}
		b = append(b, c)
	}
	return b
}
