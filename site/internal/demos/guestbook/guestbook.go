// Package guestbook is the site's first demo, lith's template on the site:
// everyone signs one book, every open tab sees each new entry in the next
// tick, and each tab sees what became of its own last signature.
package guestbook

import (
	"embed"
	"io/fs"
	"time"

	"github.com/zweiundeins/lith/site/internal/demo"
)

const (
	Slug       = "guestbook"
	MaxName    = 40              // characters
	MaxMessage = 280             // characters
	Shown      = 20              // entries on the demo's page
	Latest     = 3               // entries in the landing page's slot
	Kept       = 200             // entries the sweep keeps
	Fresh      = 8 * time.Second // how long a new entry counts as arriving
	signEvery  = 3 * time.Second // a session signs once in three seconds,
	signBurst  = 3               // after three at once
)

// Topic is touched by every change to the entries.
const Topic = Slug

// Entry is one signature.
type Entry struct {
	ID      int64
	Name    string
	Message string
	At      time.Time
}

// TabState is a tab's state in site_tabs: the op id of its last signature,
// and why that added no entry, if it did not.
type TabState struct {
	Op      string `json:"op,omitempty"`
	Refusal string `json:"refusal,omitempty"`
}

// Accepted reports whether the tab's last signature added an entry.
func (s TabState) Accepted() bool { return s.Op != "" && s.Refusal == "" }

var (
	//go:embed server.md
	server string
	//go:embed how.md
	how string
	//go:embed guestbook.css
	css string
)

// Demo is the guestbook as the site lists it.
var Demo = demo.Demo{
	Slug:       Slug,
	Title:      "Guestbook",
	Summary:    "Sign the book: the entry reaches every open tab in the next tick, and only your tab hears if it was refused.",
	Art:        art,
	Migrations: mustSub("migrations"),
	Server:     server,
	How:        how,
	CSS:        css,
	Mount:      mount,
	Page:       demo.Part{Render: renderPage, Views: pageViews},
	Embed:      &demo.Embed{Slot: demo.SlotImmediate, Part: demo.Part{Render: renderEmbed, Views: embedViews}},
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
