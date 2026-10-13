package loadtest

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"
)

// Board is the scenario of lith's own benchmarks: Boards boards of Rows
// cards each. A board is one view, keyed "board:<n>", that lists its
// cards as kanban does, in about 700 bytes of HTML each: 200 cards make
// the 140 KB of kanban's board. A command adds one to a card's counter
// and touches its board, so every command changes a view. With one row,
// each board is a counter: one view per key.
type Board struct {
	Boards int // default 1
	Rows   int // cards per board, default 200
}

//go:embed migrations/*.sql
var migrations embed.FS

// Migrations is the schema, for Config.SQLite.Migrations.
func (Board) Migrations() fs.FS {
	m, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic(err)
	}
	return m
}

// Seed fills the boards, for Config.Seed.
func (b Board) Seed() writer.Command {
	boards, rows := b.size()
	return writer.Func(func(tx *writer.Tx) error {
		for id := 1; id <= boards*rows; id++ {
			c := madeUp(int64(id))
			_, err := tx.Exec(`INSERT INTO loadtest_cards (id, title, labels, due, comments) VALUES (?, ?, ?, ?, ?)`,
				c.id, c.title, c.labels, c.due, c.comments)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// View is board n, for 0 <= n < Boards. Its cards are the rows n*Rows+1
// to (n+1)*Rows.
func (b Board) View(n int) live.View {
	_, rows := b.size()
	key := "board:" + strconv.Itoa(n)
	first, last := n*rows+1, (n+1)*rows
	return live.View{Key: key, Topics: []string{key}, Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
		cards, err := tx.QueryContext(ctx, `SELECT id, title, labels, due, comments, n FROM loadtest_cards WHERE id BETWEEN ? AND ? ORDER BY id`, first, last)
		if err != nil {
			return nil, err
		}
		defer cards.Close()
		html := make([]byte, 0, 768*rows)
		html = fmt.Appendf(html, `<div id="board-%d" class="board" aria-label="Cards">`+"\n", n)
		for cards.Next() {
			var c card
			if err := cards.Scan(&c.id, &c.title, &c.labels, &c.due, &c.comments, &c.n); err != nil {
				return nil, err
			}
			html = c.append(html, n)
		}
		if err := cards.Err(); err != nil {
			return nil, err
		}
		return sse.PatchElements(append(html, "</div>"...)), nil
	}}
}

// Stream watches board i%Boards, for Config.Stream.
func (b Board) Stream(i int) live.Spec {
	boards, _ := b.size()
	return live.Spec{Views: []live.View{b.View(i % boards)}}
}

// Command adds one to a card, for Config.Command. Its board and card
// follow from p and i alone, so a run sends the same commands every time.
func (b Board) Command(p int, i int64) writer.Command {
	boards, rows := b.size()
	x := mix(uint64(p)<<40 ^ uint64(i))
	n := int(x % uint64(boards))
	id := n*rows + int((x>>32)%uint64(rows)) + 1
	return bump{id: int64(id), topic: "board:" + strconv.Itoa(n)}
}

func (b Board) size() (boards, rows int) {
	return orDefault(b.Boards, 1), orDefault(b.Rows, 200)
}

func orDefault(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

type bump struct {
	id    int64
	topic string
}

func (c bump) Apply(tx *writer.Tx) error {
	if _, err := tx.Exec(`UPDATE loadtest_cards SET n = n + 1 WHERE id = ?`, c.id); err != nil {
		return err
	}
	tx.Touch(c.topic)
	return nil
}

type card struct {
	id, comments, n    int64
	title, labels, due string
}

var (
	colors = map[string]string{"Backend": "blue", "Frontend": "green", "Bug": "red", "Design": "purple", "Ops": "orange", "Docs": "teal"}
	names  = slices.Sorted(maps.Keys(colors))
)

// append writes the card as kanban's board does. Titles and labels are
// plain words, so nothing needs escaping.
func (c card) append(b []byte, board int) []byte {
	b = fmt.Appendf(b, `<article id="card-%[1]d" class="card" data-kanban-card="%[1]d" data-n="%[2]d" tabindex="0" aria-labelledby="card-%[1]d-t" aria-describedby="board-help"><i class="card-grip" aria-hidden="true"></i>`, c.id, c.n)
	if c.labels != "" {
		b = append(b, `<ul class="card-labels" aria-label="Labels">`...)
		for l := range strings.SplitSeq(c.labels, ",") {
			b = fmt.Appendf(b, `<li class="label l-%s">%s</li>`, colors[l], l)
		}
		b = append(b, `</ul>`...)
	}
	b = fmt.Appendf(b, `<a id="card-%[1]d-t" class="card-title" href="/boards/%[2]d/cards/%[1]d" data-card-main>%[3]s</a><div class="card-meta">`, c.id, board, c.title)
	if c.due != "" {
		b = fmt.Appendf(b, `<span class="badge due" title="Due on %[1]s"><span class="sr-only">Due </span>%[1]s</span>`, c.due)
	}
	if c.comments > 0 {
		b = fmt.Appendf(b, `<span class="badge b-comments" title="%[1]d comments">%[1]d<span class="sr-only"> comments</span></span>`, c.comments)
	}
	return fmt.Appendf(b, `<span class="badge b-count" title="Changed %[1]d times">%[1]d<span class="sr-only"> changes</span></span></div></article>`+"\n", c.n)
}

var words = strings.Fields(`move the billing export to the new api and drop the old
	session cookies before release check why safari loses the upload progress
	after a reconnect write the onboarding copy for teams review search ranking
	for long titles speed up the weekly report keep drafts when the network fails
	migrate invoices to sqlite add keyboard shortcuts for lanes`)

// madeUp is card id as it is seeded, the same on every run.
func madeUp(id int64) card {
	x := mix(uint64(id))
	ws := make([]string, 8+x%7)
	for i := range ws {
		ws[i] = words[mix(x+uint64(i))%uint64(len(words))]
	}
	c := card{id: id, title: strings.ToUpper(ws[0][:1]) + ws[0][1:] + " " + strings.Join(ws[1:], " ")}
	var ls []string
	for i := range int((x >> 8) % 4) {
		ls = append(ls, names[(int(x>>16)+i)%len(names)])
	}
	c.labels = strings.Join(ls, ",")
	if (x>>24)%3 != 0 {
		c.due = fmt.Sprintf("2026-11-%02d", 1+(x>>32)%30)
	}
	if (x>>40)%3 != 0 {
		c.comments = int64(1 + (x>>48)%9)
	}
	return c
}

// mix is splitmix64's finalizer: a different, evenly spread number for
// every input.
func mix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ x>>30) * 0xbf58476d1ce4e5b9
	x = (x ^ x>>27) * 0x94d049bb133111eb
	return x ^ x>>31
}
