// Package town is a small town of tiles that everyone builds together,
// event-sourced on lith/events. Each tile is a stream; placing a house, a
// tree, a road or water on it is a command that a decider turns into an
// event. One inline projector keeps the town as it is now, another a
// history table with the positions between which each thing stood. A
// slider takes each tab back to any position of the log: the tab's
// position is its state in SQLite, and the town as of a position is a view
// of its own, which every tab at that position shares and no new event
// renders again.
package town

import (
	"embed"
	"io/fs"
	"strconv"
	"time"

	"github.com/zweiundeins/lith/site/internal/demo"
)

const (
	Slug = "town"
	// Cols and Rows are the town's size in tiles; a tile's number counts
	// from 0, left to right and top to bottom.
	Cols  = 12
	Rows  = 8
	Tiles = Cols * Rows
	// Kept is how many events the sweep keeps, the newest, besides a
	// snapshot of each tile from before them: the slider starts at the
	// newest snapshot.
	Kept = 5000
	// Earlier is how many events the page lists up to its position, the
	// one at the position first; Later how many after it.
	Earlier = 5
	Later   = 3

	placeEvery, placeBurst   = 200 * time.Millisecond, 10 // a session's changes: five a second, after ten at once
	seekEvery, seekBurst     = 25 * time.Millisecond, 80  // a session's slider: 40 positions a second, after 80
	followEvery, followBurst = time.Second, 5             // a stream's hello
	// Throttle is how often a dragged slider sends its position at most:
	// once a tick, and the last one where it comes to rest.
	Throttle = 50 * time.Millisecond
)

// The topics: Topic for every change to the town, TopicPruned when the
// sweep dropped old events, which moves the slider's start. TopicPruned
// is under Topic: every view on Topic listens to it as well.
const (
	Topic       = Slug
	TopicPruned = Slug + ":pruned"
)

// The views' keys. The town now is one view; the town as of a position
// is one per position (atKey), as are the events after it (laterKey).
const (
	nowKey  = Slug + ":now"
	headKey = Slug + ":head"
)

func atKey(p int64) string    { return Slug + ":at:" + strconv.FormatInt(p, 10) }
func laterKey(p int64) string { return Slug + ":later:" + strconv.FormatInt(p, 10) }

// TabState is a tab's state in site_tabs: the position it shows (0: now)
// with the number of the seek that put it there, and its last change of a
// tile with what became of it.
type TabState struct {
	At      int64  `json:"at,omitempty"`
	Seq     int64  `json:"seq,omitempty"`
	Op      string `json:"op,omitempty"`
	Tile    int    `json:"tile,omitempty"`
	Refusal string `json:"refusal,omitempty"`
}

var (
	//go:embed server.md
	server string
	//go:embed how.md
	how string
	//go:embed town.css
	css string
	//go:embed migrations
	migrationFiles embed.FS
)

// Demo is the town as the site lists it.
var Demo = demo.Demo{
	Slug:       Slug,
	Title:      "Town in time",
	Featured:   true,
	Summary:    "Build a small town together: every tile you change is an event, and a slider takes your tab back to any of them while the others build on.",
	Art:        art,
	Migrations: mustSub("migrations"),
	Server:     server,
	How:        how,
	CSS:        css,
	Events:     Events,
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
