// Package cursorparty is the cursor party: every visitor's pointer moves
// live behind the landing page's title and on the demo's own page. A tab
// posts its pointer's position about 15 times a second, the writer keeps
// one row per tab, and one shared view draws every live cursor as one
// layer per tick. A tab's own cursor is never drawn back to it: its page
// hides it by the id a hash of its tab id gives.
package cursorparty

import (
	"embed"
	"io/fs"
	"sync/atomic"
	"time"

	"github.com/zweiundeins/lith/live"

	"github.com/zweiundeins/lith/site/internal/demo"
)

const (
	Slug       = "cursor-party"
	Shown      = 64                     // cursors in the layer at most: the newest
	Idle       = 3 * time.Second        // a cursor that has not moved for this long fades
	Gone       = 6 * time.Second        // and leaves the layer after this long
	Keep       = 2 * time.Minute        // the rows of tabs quiet this long go
	MaxCursors = 1000                   // rows at most: the oldest go first
	Throttle   = 67 * time.Millisecond  // the page sends a move this often at most: 15 a second
	Rest       = 200 * time.Millisecond // and the one where the pointer rests, this long after
	moveEvery  = 50 * time.Millisecond  // the server's limit: 20 moves a second per session,
	moveBurst  = 40                     // after 40 at once
)

// Topic is touched by every move.
const Topic = Slug

// The views' keys: the same on the landing page and the demo's page, so
// one render a tick serves both.
const (
	cursorsKey = Slug + ":cursors"
	countKey   = Slug + ":count"
)

var (
	//go:embed server.md
	server string
	//go:embed how.md
	how string
	//go:embed cursor-party.css
	css string
)

// Demo is the cursor party as the site lists it.
var Demo = demo.Demo{
	Slug:       Slug,
	Title:      "Cursor party",
	Summary:    "Every visitor's pointer, live: each tab sends its position 15 times a second, and one view draws them all once per tick.",
	Art:        art,
	Migrations: mustSub("migrations"),
	Server:     server,
	How:        how,
	CSS:        css,
	Components: []string{"sparkline"},
	Mount:      mount,
	Page:       demo.Part{Render: renderPage, Views: views},
	Embed:      &demo.Embed{Slot: demo.SlotHero, Part: demo.Part{Render: renderEmbed, Views: views}},
	Sweep:      sweep,
}

// hub is the hub of the site that mounted the demo: the counter asks it
// how many streams watch the party. Nil in tests without a site.
var hub atomic.Pointer[live.Hub]

//go:embed migrations
var migrationFiles embed.FS

func mustSub(dir string) fs.FS {
	f, err := fs.Sub(migrationFiles, dir)
	if err != nil {
		panic(err)
	}
	return f
}
