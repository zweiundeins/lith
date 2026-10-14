package life

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/fnv"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"
)

// Board is the board's row: the cells and what the clock keeps beside
// them.
type Board struct {
	Cells      []byte
	Generation int64
	Stepped    int64  // unix ms of the newest generation
	Rate       int    // generations a second, in tenths
	RateSince  int64  // unix ms: the start of the second the rate counts
	RateFrom   int64  // the generation then
	Quiet      int64  // unix ms since the board is empty or repeats itself; 0 while it lives
	Trail      []byte // what died a generation or two ago: age*16 + colour, or empty
	Recent     []byte // hashes of the last four generations
	History    string // the population once a second: a JSON array
	Reset      int64  // unix ms of the last reset
}

const selectBoard = `SELECT cells, trail, generation, stepped, rate, rate_since, rate_from, quiet, recent, history, reset FROM life_board WHERE id = 1`

// load reads the board, in a command, a view or a GET.
func load(ctx context.Context, q sqlite.Querier) (Board, error) {
	var b Board
	err := q.QueryRowContext(ctx, selectBoard).Scan(&b.Cells, &b.Trail, &b.Generation, &b.Stepped, &b.Rate, &b.RateSince, &b.RateFrom, &b.Quiet, &b.Recent, &b.History, &b.Reset)
	if err == nil && len(b.Cells) != Cells {
		err = errors.New("life: the board has the wrong size")
	}
	return b, err
}

// save writes the board and touches its topic.
func (b *Board) save(tx *writer.Tx) error {
	if b.Recent == nil {
		b.Recent = []byte{} // nil would be NULL
	}
	if b.Trail == nil {
		b.Trail = []byte{}
	}
	_, err := tx.Exec(`UPDATE life_board SET cells = ?, trail = ?, generation = ?, stepped = ?, rate = ?, rate_since = ?, rate_from = ?, quiet = ?, recent = ?, history = ?, reset = ? WHERE id = 1`,
		b.Cells, b.Trail, b.Generation, b.Stepped, b.Rate, b.RateSince, b.RateFrom, b.Quiet, b.Recent, b.History, b.Reset)
	if err != nil {
		return err
	}
	tx.Touch(Topic)
	return nil
}

// step computes the next generation at now (unix ms), and notes whether
// the board has gone quiet: empty, or the same as one of the four
// generations before it.
func (b *Board) step(now int64) {
	next := make([]byte, Cells)
	population := Next(next, b.Cells, b.Generation)
	b.Trail = trail(b.Trail, b.Cells, next)
	b.Cells = next
	b.Generation++
	h := Hash(next)
	repeats := false
	for i := 0; i+8 <= len(b.Recent); i += 8 {
		repeats = repeats || binary.LittleEndian.Uint64(b.Recent[i:]) == h
	}
	b.Recent = binary.LittleEndian.AppendUint64(b.Recent, h)
	if len(b.Recent) > 32 {
		b.Recent = slices.Clone(b.Recent[len(b.Recent)-32:])
	}
	switch {
	case population > 0 && !repeats:
		b.Quiet = 0
	case b.Quiet == 0:
		b.Quiet = now
	}
	// The rate counts the generations of the last full second. A gap of a
	// second or more (nobody watched) starts the count again.
	switch {
	case b.Stepped == 0 || now-b.Stepped >= 1000:
		b.RateSince, b.RateFrom = now, b.Generation
	case now-b.RateSince >= 1000:
		b.Rate = int((b.Generation - b.RateFrom) * 10_000 / (now - b.RateSince))
		b.RateSince, b.RateFrom = now, b.Generation
		b.sample(population)
	}
	b.Stepped = now
}

// trail ages the trail by a generation from cur to next: a cell that dies
// leaves its colour for two generations, as the page draws it shrinking.
func trail(old, cur, next []byte) []byte {
	t := make([]byte, Cells)
	for i, c := range next {
		switch {
		case c != 0:
		case cur[i] != 0:
			t[i] = 1<<4 | cur[i]
		case len(old) == Cells && old[i]>>4 == 1:
			t[i] = 2<<4 | old[i]&15
		}
	}
	return t
}

// sample adds the population to the history, which keeps the last
// History samples.
func (b *Board) sample(population int) {
	var h []int
	json.Unmarshal([]byte(b.History), &h)
	h = append(h, population)
	if len(h) > History {
		h = h[len(h)-History:]
	}
	out, _ := json.Marshal(h)
	b.History = string(out)
}

// changed marks a board that people changed at now: it lives again, unless
// they cleared it, and what it repeated before is forgotten.
func (b *Board) changed(now int64) {
	b.Recent = binary.LittleEndian.AppendUint64(nil, Hash(b.Cells))
	b.Quiet = 0
	if Population(b.Cells) == 0 {
		b.Quiet = now
	}
}

// quietFor is how long a quiet board waits for a new soup.
func (b *Board) quietFor() time.Duration {
	if Population(b.Cells) == 0 {
		return EmptyFor
	}
	return SettledFor
}

// reseed puts a new soup on the board at now, from a source seeded by now.
func (b *Board) reseed(now int64) {
	b.Cells, b.Trail = make([]byte, Cells), nil
	Soup(b.Cells, rand.New(rand.NewPCG(uint64(now), 0x11fe)))
	b.restart()
	b.Quiet, b.Recent = 0, nil
}

// restart counts the generations, and their rate, from 0 again.
func (b *Board) restart() {
	b.Generation, b.Stepped = 0, 0
	b.Rate, b.RateSince, b.RateFrom = 0, 0, 0
}

// Counts are the live cells of each colour, the soup's last.
func Counts(cells []byte) (n [Colours + 2]int) {
	for _, c := range cells {
		n[c]++
	}
	return n
}

// colourOf is the colour a session draws in: the one it picked, or else
// the one its id picks.
func colourOf(ctx context.Context, q sqlite.Querier, sid string) (int, error) {
	var c int
	err := q.QueryRowContext(ctx, `SELECT colour FROM life_players WHERE sid = ?`, sid).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultColour(sid), nil
	}
	return c, err
}

// defaultColour is the colour a session's id picks, 1 to Colours.
func defaultColour(sid string) int {
	h := fnv.New32a()
	h.Write([]byte(sid))
	return 1 + int(h.Sum32()%Colours)
}
