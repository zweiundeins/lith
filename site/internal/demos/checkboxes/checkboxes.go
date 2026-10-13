// Package checkboxes is a million checkboxes that everyone shares. The
// boxes live in SQLite, 256 bits to a row. The page shows them in tiles of
// 256 inside Starbase's sb-virtual-scroll, which holds only the tiles near
// the view. Each tile is a view of its own, shared by every tab that shows
// it, and a tab watches the tiles of its window and no others: a tick is
// enough for a flip to reach everyone who has the box in view.
package checkboxes

import (
	"embed"
	"io/fs"
	"strconv"
	"time"

	"github.com/zweiundeins/lith/site/internal/demo"
)

const (
	Slug = "checkboxes"
	// Boxes is how many boxes there are, numbered 1 to Boxes on the page
	// and 0 to Boxes-1 inside.
	Boxes = 1_000_000
	// ChunkSize is how many boxes a chunk holds: a row of
	// checkboxes_chunks, a tile on the page, a view.
	ChunkSize = 256
	// Chunks is how many chunks there are; the last one holds 64 boxes.
	Chunks = (Boxes + ChunkSize - 1) / ChunkSize
	// Slots is how many chunks a tab watches at once: the window around
	// what it shows. The page has as many slots for tiles, and chunk n's
	// tile goes into slot n % Slots, so its frame finds its place in every
	// tab that shows it.
	Slots = 12
	// MaxCols is the most tiles in a row of the grid.
	MaxCols = 4
	// Trend is how many seconds the rate's chart shows.
	Trend = 30
	// KeptSeconds is how long the per-second counts are kept.
	KeptSeconds = time.Hour

	setEvery, setBurst       = 100 * time.Millisecond, 30 // a session's clicks: 10 a second, after 30 at once
	windowEvery, windowBurst = 100 * time.Millisecond, 30 // a session's windows, as it scrolls
	// rewatchAfter is how long after its stream opens a tab's window is
	// watched again: long enough for the stream to have replaced one
	// still open with the tab's id.
	rewatchAfter = 250 * time.Millisecond
)

// StatsTopic is touched by every flip.
const StatsTopic = Slug + ":stats"

// ChunkTopic is touched by every flip in chunk n; it is also the key of
// the chunk's view.
func ChunkTopic(n int) string { return Slug + ":chunk:" + strconv.Itoa(n) }

var (
	//go:embed server.md
	server string
	//go:embed how.md
	how string
	//go:embed checkboxes.css
	css string
)

// Demo is the million checkboxes as the site lists it.
var Demo = demo.Demo{
	Slug:       Slug,
	Title:      "A million checkboxes",
	Summary:    "One grid of a million boxes for everyone: tick one, and every tab that has it in view sees it flip within a tick.",
	Art:        art,
	Migrations: mustSub("migrations"),
	Server:     server,
	How:        how,
	CSS:        css,
	Components: []string{"virtual-scroll", "sparkline"},
	Mount:      mount,
	Page:       demo.Part{Render: renderPage, Views: pageViews},
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
