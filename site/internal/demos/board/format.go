package board

import (
	"context"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
)

func cardID(id int64) string { return "board-card-" + strconv.FormatInt(id, 10) }

// landed reports whether c is the card someone moved last, moments ago:
// it glows as it lands, and lifts while it flies (board.css).
func landed(b Board, c Card, now time.Time) bool {
	return b.LastBy != "" && b.Last.ID == c.ID && now.Sub(c.MovedAt) < Fresh
}

func cardsWord(n int) string {
	if n == 1 {
		return " card"
	}
	return " cards"
}

func hueStyle(a Avatar) string { return "--h: " + strconv.Itoa(a.Hue) }

// meStyle marks the viewer's own avatar, wherever the stream puts it: the
// shared views carry nothing about who looks at them.
func meStyle(me string) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if !validWho(me) {
			return nil
		}
		_, err := io.WriteString(w, `<style>[data-demo="board"] [data-who="`+me+`"]{--bd-me:1;--bd-you:" (you)"}</style>`)
		return err
	})
}

// hereLine counts the people on the board, and their tabs when some
// have more than one open.
func hereLine(people []Person) string {
	tabs := 0
	for _, p := range people {
		tabs += p.Tabs
	}
	switch {
	case len(people) == 0:
		return "Nobody on the board yet"
	case len(people) == 1 && tabs == 1:
		return "1 person on the board"
	case len(people) == 1:
		return "1 person, " + strconv.Itoa(tabs) + " tabs"
	case tabs == len(people):
		return strconv.Itoa(len(people)) + " people on the board"
	}
	return strconv.Itoa(len(people)) + " people, " + strconv.Itoa(tabs) + " tabs"
}

// ceiling is the sparkline's top: the series' peak rounded up to 1, 2 or
// 5 times a power of ten, and at least 5, so that a quiet board draws a
// flat line at the bottom.
func ceiling(s []int) int {
	peak := 5
	for _, v := range s {
		peak = max(peak, v)
	}
	for step := 1; ; step *= 10 {
		for _, m := range []int{1, 2, 5} {
			if m*step >= peak {
				return m * step
			}
		}
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func rate(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }

// series is the sparkline's values attribute, a JSON array.
func series(s []int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range s {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(v))
	}
	b.WriteByte(']')
	return b.String()
}

// miniLabel says what the miniature shows, for screen readers.
func miniLabel(b Board) string {
	parts := make([]string, Lanes)
	for i, lane := range b.Lanes {
		parts[i] = strconv.Itoa(len(lane)) + " " + strings.ToLower(LaneNames[i])
	}
	return "The shared board: " + strings.Join(parts, ", ")
}
