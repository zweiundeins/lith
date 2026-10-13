// Package canvas is the shared pixel canvas: everyone paints on one 64 by
// 64 board with Starbase's sb-pixel-board. A stroke is a run of commands;
// the writer keeps the board as one row of 2,048 bytes, and one shared view
// renders it once per tick for every painter. The page lists the painters
// here in their colours and counts the pixels of the last minute. A pixel
// fades back to the background a day after it was last painted.
package canvas

import (
	"embed"
	"io/fs"
	"time"

	"github.com/zweiundeins/lith/site/internal/demo"
)

const (
	Slug     = "canvas"
	Size     = 64             // cells per side
	Cells    = Size * Size    // cells on the board
	MaxPaint = 60             // cells in one paint, the batch sb-pixel-board sends at most
	Lifetime = 24 * time.Hour // a pixel fades back to the background this long after its last paint
	Window   = 60             // seconds in "the last minute"
	Shown    = 40             // painters the list shows at most

	// MaxPainters is how many painters the sweep keeps at most, the
	// newest: a painter is a row per tab that opened the page in the last
	// day.
	MaxPainters = 5000

	// The pixel limits: a session paints 20 pixels a second after a burst
	// of 60, an address four times that.
	pixelRate  = 20
	pixelBurst = 60
	// A stroke posts a batch every 80 ms: 12.5 requests a second.
	paintEvery = 40 * time.Millisecond
	paintBurst = 25
	pickEvery  = 200 * time.Millisecond
	pickBurst  = 10

	// DefaultColor is the brush a new tab starts with: the seam's ember.
	DefaultColor = 7
)

// The topics the commands touch.
const (
	TopicCells    = Slug + ":cells"    // a cell changed colour
	TopicPaints   = Slug + ":paints"   // pixels were painted or faded
	TopicPainters = Slug + ":painters" // a painter came, or changed colour
)

var (
	//go:embed server.md
	server string
	//go:embed how.md
	how string
	//go:embed canvas.css
	css string
	//go:embed migrations
	migrationFiles embed.FS
)

// Demo is the canvas as the site lists it.
var Demo = demo.Demo{
	Slug:       Slug,
	Title:      "Canvas",
	Summary:    "Paint on one pixel canvas with everyone here: each stroke is a command, and one render a tick carries the board to every painter.",
	Art:        art,
	Migrations: mustSub("migrations"),
	Server:     server,
	How:        how,
	CSS:        css,
	Components: []string{"pixel-board"},
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
