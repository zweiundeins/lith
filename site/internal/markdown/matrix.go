package markdown

import (
	"errors"
	"fmt"
	"html"
	"strings"
)

// Matrix is a ```matrix block: a table that compares rows (approaches)
// across columns (cases), each cell rated good, fair or poor. Lines of
// key: value, a line with ---, then a row per line, its name and cells
// separated by |.
//
//	```matrix
//	title: A page too slow for its stream
//	source: bench/drift/RESULTS.md bench/hybrid/RESULTS.md
//	columns: Slow phone, fast link | Fast phone, far link
//	---
//	No fix | poor: 27.15 s behind | good: 10.07 frames a second
//	`newest-frame.js` | fair: 0.62 s behind | not measured
//	```
//
// A cell is "good: text", "fair: text" or "poor: text", or "not
// measured" or "measuring" alone. The state shows as a word in the cell
// as well as its colour. source lists the files the numbers come from,
// relative to lith's repository; the docs' tests check that every number
// of a cell appears in one of them. note, if set, goes under the table.
type Matrix struct {
	Title, Note string
	Sources     []string
	Columns     []string
	Rows        []MatrixRow
}

// MatrixRow is a row of a matrix: its name and a cell per column.
type MatrixRow struct {
	Name  string
	Cells []MatrixCell
}

// MatrixCell is a cell: its state (good, fair, poor, or none for a cell
// not measured yet) and its text, as written.
type MatrixCell struct{ State, Text string }

var matrixStates = map[string]string{"good": "Good", "fair": "Fair", "poor": "Poor"}

// ParseMatrix reads the body of a ```matrix block.
func ParseMatrix(src string) (*Matrix, error) {
	head, data, ok := strings.Cut(src, "\n---\n")
	if !ok {
		return nil, errors.New("matrix: no --- line between the spec and the rows")
	}
	m := &Matrix{}
	for line := range strings.Lines(head) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("matrix: %q is not key: value", line)
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "title":
			m.Title = v
		case "source":
			m.Sources = strings.Fields(v)
		case "columns":
			m.Columns = splitCells(v)
		case "note":
			m.Note = v
		default:
			return nil, fmt.Errorf("matrix: unknown key %q", k)
		}
	}
	if m.Title == "" || len(m.Sources) == 0 || len(m.Columns) == 0 {
		return nil, errors.New("matrix: a matrix needs a title, a source and columns")
	}
	for line := range strings.Lines(data) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := splitCells(line)
		if len(fields) != len(m.Columns)+1 {
			return nil, fmt.Errorf("matrix %q: row %q has %d cells, want %d", m.Title, fields[0], len(fields)-1, len(m.Columns))
		}
		row := MatrixRow{Name: fields[0]}
		for _, f := range fields[1:] {
			c, err := parseCell(f)
			if err != nil {
				return nil, fmt.Errorf("matrix %q, row %q: %w", m.Title, row.Name, err)
			}
			row.Cells = append(row.Cells, c)
		}
		m.Rows = append(m.Rows, row)
	}
	if len(m.Rows) == 0 {
		return nil, fmt.Errorf("matrix %q: no rows", m.Title)
	}
	return m, nil
}

func splitCells(s string) []string {
	fields := strings.Split(s, "|")
	for i, f := range fields {
		fields[i] = strings.TrimSpace(f)
	}
	return fields
}

func parseCell(s string) (MatrixCell, error) {
	switch s {
	case "not measured", "measuring":
		return MatrixCell{Text: s}, nil
	}
	state, text, ok := strings.Cut(s, ":")
	if !ok || matrixStates[state] == "" {
		return MatrixCell{}, fmt.Errorf("cell %q: want good:, fair: or poor:, or not measured or measuring", s)
	}
	return MatrixCell{State: state, Text: strings.TrimSpace(text)}, nil
}

// HTML renders the matrix as a figure: a caption with its title and
// sources, and the table, which scrolls sideways in a narrow column.
func (m *Matrix) HTML() string {
	var b strings.Builder
	b.WriteString(`<figure class="chart matrix">`)
	b.WriteString(`<figcaption class="chart__bar"><span class="chart__title">`)
	b.WriteString(html.EscapeString(m.Title))
	b.WriteString(`</span><span class="chart__source">`)
	for i, s := range m.Sources {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, `<a href="%s">%s</a>`, html.EscapeString(SourceURL(s)), html.EscapeString(s))
	}
	b.WriteString(`</span></figcaption>`)
	b.WriteString(`<div class="matrix__scroll" tabindex="0"><table class="matrix__table"><thead><tr><td></td>`)
	for _, c := range m.Columns {
		fmt.Fprintf(&b, `<th scope="col">%s</th>`, inline(c))
	}
	b.WriteString(`</tr></thead><tbody>`)
	for _, r := range m.Rows {
		fmt.Fprintf(&b, `<tr><th scope="row">%s</th>`, inline(r.Name))
		for _, c := range r.Cells {
			if c.State == "" {
				fmt.Fprintf(&b, `<td class="matrix__cell matrix__cell--none"><span class="matrix__text">%s</span></td>`, inline(capital(c.Text)))
				continue
			}
			fmt.Fprintf(&b, `<td class="matrix__cell matrix__cell--%s"><span class="matrix__state">%s</span> <span class="matrix__text">%s</span></td>`, c.State, matrixStates[c.State], inline(c.Text))
		}
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table></div>`)
	if m.Note != "" {
		fmt.Fprintf(&b, `<p class="matrix__note">%s</p>`, inline(m.Note))
	}
	b.WriteString("</figure>\n")
	return b.String()
}

// inline escapes s and sets its `code spans` as code.
func inline(s string) string {
	parts := strings.Split(s, "`")
	var b strings.Builder
	for i, p := range parts {
		if i%2 == 1 && i < len(parts)-1 {
			b.WriteString("<code>" + html.EscapeString(p) + "</code>")
			continue
		}
		if i%2 == 1 {
			b.WriteString("`")
		}
		b.WriteString(html.EscapeString(p))
	}
	return b.String()
}

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
