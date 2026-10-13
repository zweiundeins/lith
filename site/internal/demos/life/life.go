// Package life is Conway's Game of Life as a demo of lith: one board in
// SQLite, shared by everyone on the page. While anyone watches, a clock
// sends a command ten times a second that computes the next generation;
// the board's view renders once per generation for every viewer and goes
// out as one fat morph, which zstd shrinks to a few hundred bytes per
// stream. Visitors draw cells and drop patterns in their colour, through
// rate-limited commands.
package life

import (
	"embed"
	"io/fs"
	"time"

	"github.com/zweiundeins/lith/site/internal/demo"
)

const (
	Slug = "life"
	// Speed is how many generations the clock asks for in a second while
	// someone watches.
	Speed = 10
	// Idle is how often the clock looks for viewers while no one watches.
	Idle = 250 * time.Millisecond
	// EmptyFor and SettledFor are how long the board stays empty, or
	// repeating itself (still lifes and blinkers), before it gets a new
	// soup.
	EmptyFor   = 15 * time.Second
	SettledFor = 30 * time.Second
	// ResetEvery is how long a reset by anyone holds before the next one.
	ResetEvery = 10 * time.Second
	// MaxStroke is the most points one draw command takes; lines join
	// them.
	MaxStroke = 64
	// History is how many samples of the population the board keeps, one
	// a second.
	History = 60
	// PlayerTTL is how long a session's chosen colour lasts, and
	// MaxPlayers how many the sweep keeps.
	PlayerTTL  = 30 * 24 * time.Hour
	MaxPlayers = 10_000
)

// Topic is touched by every change to the board; BoardKey is the shared
// view of it, whose watchers the clock counts.
const (
	Topic    = Slug
	BoardKey = Slug + ":board"
)

// sessionTopic is touched when a session picks its colour, so that each of
// its tabs shows the new one.
func sessionTopic(sid string) string { return Slug + ":sid:" + sid }

// TabState is a tab's state in site_tabs: its newest action, and why the
// board refused it, if it did.
type TabState struct {
	Op      string `json:"op,omitempty"`
	Refusal string `json:"refusal,omitempty"`
}

var (
	//go:embed server.md
	server string
	//go:embed how.md
	how string
	//go:embed life.css
	css string
	//go:embed migrations
	migrationFiles embed.FS
)

// Demo is the Game of Life as the site lists it.
var Demo = demo.Demo{
	Slug:       Slug,
	Title:      "Game of Life",
	Summary:    "One board for everyone: the server computes ten generations a second while anyone watches, and every tab gets the same frame.",
	Art:        art,
	Migrations: mustSub("migrations"),
	Server:     server,
	How:        how,
	CSS:        css,
	Components: []string{"sparkline"},
	Mount:      mount,
	Page:       demo.Part{Render: renderPage, Views: pageViews},
	Sweep:      sweep,
}

func mustSub(dir string) fs.FS {
	f, err := fs.Sub(migrationFiles, dir)
	if err != nil {
		panic(err)
	}
	return f
}
