// Package kanban is the workload both stores run: one board of 200 cards in
// four lanes, and commands that move a card if it has not changed since the
// mover saw it.
package kanban

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	Cards = 200
	Lanes = 4
	// StaleEvery makes every 50th command of a producer carry an outdated
	// version, so 2% of the commands are refused on every store alike.
	StaleEvery = 50
)

// ErrMoved refuses a move whose card changed since the mover saw it.
var ErrMoved = errors.New("card moved meanwhile")

// Card is one row of the board as a render reads it.
type Card struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Lane    int64  `json:"lane"`
	Pos     int64  `json:"pos"`
	Version int64  `json:"version"`
}

// Seed is card i (0-based) as it starts.
func Seed(i int) Card {
	words := strings.Fields("move the billing export to the new api and drop old session cookies before release check why safari loses upload progress")
	title := fmt.Sprintf("%s %s %s %d", words[i%len(words)], words[(i*7+3)%len(words)], words[(i*13+5)%len(words)], i+1)
	return Card{Title: title, Lane: int64(i % Lanes), Pos: int64(i / Lanes), Version: 0}
}

// Owned lists the cards (0-based) producer p of n moves: each card has one
// mover, as lith's writer benchmark gives each producer its own accounts.
func Owned(p, n int) []int {
	var cards []int
	for c := p; c < Cards; c += n {
		cards = append(cards, c)
	}
	return cards
}

// Move is one command: card (0-based), the version the mover saw and the
// lane it moves the card to.
type Move struct {
	Card       int
	Seen, Lane int64
}

// Mover walks a producer's cards, one move at a time, and keeps the
// versions it has seen.
type Mover struct {
	cards   []int
	version map[int]int64
	lane    map[int]int64
	i       int
}

func NewMover(p, n int) *Mover {
	m := &Mover{cards: Owned(p, n), version: map[int]int64{}, lane: map[int]int64{}}
	for _, c := range m.cards {
		m.lane[c] = Seed(c).Lane
	}
	return m
}

// Next is the next move; every StaleEvery-th carries an outdated version.
func (m *Mover) Next() Move {
	c := m.cards[m.i%len(m.cards)]
	seen := m.version[c]
	if m.i%StaleEvery == StaleEvery-1 {
		seen--
	}
	m.i++
	return Move{Card: c, Seen: seen, Lane: (m.lane[c] + 1) % Lanes}
}

// Done records the outcome of mv.
func (m *Mover) Done(mv Move, err error) {
	if err == nil {
		m.version[mv.Card] = mv.Seen + 1
		m.lane[mv.Card] = mv.Lane
	}
}

// Result is one measured run, printed as a JSON line.
type Result struct {
	Store, Mode, Bench string
	Producers          int
	Seconds            float64
	Commands           int64
	Refused            int64
	Conflicts          int64 `json:",omitempty"`
	Retried            int64 `json:",omitempty"`
	Transactions       int64
	CmdsPerSec         float64
	CmdsPerTx          float64
	P50ms, P99ms       float64
	Extra              map[string]float64 `json:",omitempty"`
}

func (r Result) Print() {
	b, _ := json.Marshal(r)
	fmt.Fprintln(os.Stdout, string(b))
}

// Percentiles returns the 50th and 99th percentile of ds in milliseconds.
func Percentiles(ds []time.Duration) (p50, p99 float64) {
	if len(ds) == 0 {
		return 0, 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	at := func(q float64) float64 {
		return float64(s[min(len(s)-1, int(q*float64(len(s))))]) / 1e6
	}
	return at(0.50), at(0.99)
}

// Window is the measured part of a run: Warm before Start does not count.
type Window struct {
	Start, End time.Time
}

func (w Window) In(t time.Time) bool { return !t.Before(w.Start) && t.Before(w.End) }
