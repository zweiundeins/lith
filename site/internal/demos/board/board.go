// Package board is the site's shared board: three lanes and a dozen cards
// that everyone moves at once. A move is a command that carries the
// version of the card its mover saw, so of two moves of the same card the
// later one is refused, and only its tab hears why. Every move renders the
// board once, in the next tick, for every viewer, and the cards glide to
// their new places in a view transition.
package board

import (
	"embed"
	"io/fs"
	"time"

	"github.com/zweiundeins/lith/site/internal/demo"
)

const (
	Slug = "board"
	// Lanes is how many lanes the board has: to do, doing, done.
	Lanes = 3
	// Quiet is how long the board may go without a move before the sweep
	// puts every card back where it started.
	Quiet = time.Hour
	// Fresh is how long a moved card counts as just arrived, for the
	// landing page's miniature.
	Fresh = 6 * time.Second
	// Shown is how many avatars the presence row shows; the rest are a
	// number.
	Shown = 12

	moveEvery  = 250 * time.Millisecond // a session moves four cards a second,
	moveBurst  = 8                      // after eight at once
	helloEvery = 2 * time.Second        // a session's tabs say hello once in two seconds,
	helloBurst = 6                      // after six at once

	// helloGrace is how long the sweep keeps a hello whose tab has no
	// open stream: the hello can arrive before the stream opens, and a
	// tab reconnects after a deploy.
	helloGrace = 10 * time.Minute
	// keptHellos bounds board_here between sweeps' checks of the streams.
	keptHellos = 2000
)

// The topics: Topic for the cards (every move and reset), HereTopic for
// the tabs on the page. HereTopic is not under Topic, or every hello would
// render the board.
const (
	Topic     = Slug
	HereTopic = "here:" + Slug
)

// The views' keys.
const (
	boardKey = Slug + ":board"
	statsKey = Slug + ":stats"
	hereKey  = Slug + ":here"
	miniKey  = Slug + ":mini"
)

// LaneNames are the lanes' names, in their order on the board.
var LaneNames = [Lanes]string{"To do", "Doing", "Done"}

// Card is one card on the board.
type Card struct {
	ID         int64
	Title, Tag string
	Lane, Pos  int
	// Version grows with every move of the card.
	Version int64
	// MovedBy is who moved it last, an avatar's seed; "" since the start.
	MovedBy string
	MovedAt time.Time
}

// Board is the board as everyone sees it.
type Board struct {
	// Version grows with every move and every reset.
	Version int64
	Lanes   [Lanes][]Card
	// Last is the card moved last, LastBy who moved it ("" when no card
	// has moved since the last reset), MovedAt when.
	Last    Card
	LastBy  string
	MovedAt time.Time
}

// TabState is a tab's state in site_tabs: its last move and what became
// of it.
type TabState struct {
	Op    string `json:"op,omitempty"`
	Card  int64  `json:"card,omitempty"`
	Title string `json:"title,omitempty"`
	// Lane and Pos are where the card is after the move: where it went,
	// or, refused, where it stays.
	Lane int `json:"lane"`
	Pos  int `json:"pos"`
	// Version is the board's version once the move is applied: the page
	// ends its pending state when its board shows that version.
	Version int64  `json:"v"`
	Refusal string `json:"refusal,omitempty"`
}

var (
	//go:embed server.md
	server string
	//go:embed how.md
	how string
	//go:embed board.css
	css string
)

// Demo is the board as the site lists it.
var Demo = demo.Demo{
	Slug:       Slug,
	Title:      "Shared board",
	Summary:    "Drag a card to another lane: the board renders once in the next tick, and every open tab sees it glide there.",
	Art:        art,
	Migrations: mustSub("migrations"),
	Server:     server,
	How:        how,
	CSS:        css,
	Components: []string{"sparkline"},
	Mount:      mount,
	Page:       demo.Part{Render: renderPage, Views: pageViews},
	Embed:      &demo.Embed{Slot: demo.SlotTicks, Part: demo.Part{Render: renderEmbed, Views: embedViews}},
	Sweep:      sweep,
}

//go:embed migrations
var migrationFiles embed.FS

func mustSub(dir string) fs.FS {
	f, err := fs.Sub(migrationFiles, dir)
	if err != nil {
		panic(err)
	}
	return f
}
