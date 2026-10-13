// Package board is the app the docs' guides follow: boards of lanes that
// hold cards, on lith. Commands (commands.go) are the only writes, queries
// (queries.go) read in the transaction they are given, and a tab's own
// state is a row of its own (tabs.go). It compiles against the lith of
// the site's commit, and its tests run it.
package board

import (
	"errors"
	"strconv"
	"time"
)

// Topic is touched by every change to a board's lanes and cards.
func Topic(board int64) string { return "board:" + strconv.FormatInt(board, 10) }

// TabTopic is touched by every change to a tab's own state.
func TabTopic(tab string) string { return "tab:" + tab }

// SessionTopic is touched by every change to a session's preferences.
func SessionTopic(sid string) string { return "sid:" + sid }

// CardTopic is touched by every change to one card, for its panel.
func CardTopic(card int64) string { return "card:" + strconv.FormatInt(card, 10) }

// Card is a card on a board.
type Card struct {
	ID      int64
	Lane    int64
	Title   string
	Version int64
}

// Lane is a lane with its cards, in the order they were added.
type Lane struct {
	ID    int64
	Name  string
	Max   int
	Cards []Card
}

// Board is a board with its lanes.
type Board struct {
	ID    int64
	Name  string
	Lanes []Lane
}

// ErrNotFound is returned for a board, lane or card that does not exist.
var ErrNotFound = errors.New("board: not found")

const (
	MaxTitle = 200                // characters in a card's title
	TabTTL   = 7 * 24 * time.Hour // how long a tab's row outlives its last action
)
