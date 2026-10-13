// Package writerrace is the writer race: up to 200 producers send small
// commands to a writer of the demo's own for three seconds, and every tab
// on the page watches its numbers tick by, from writer.Stats. One race
// runs at a time on the whole site; the others wait in line.
package writerrace

import (
	"embed"
	"io/fs"
	"slices"
	"time"

	"github.com/zweiundeins/lith/site/internal/demo"
)

const (
	Slug     = "writer-race"
	Duration = 3 * time.Second        // how long the producers of a race send
	Every    = 50 * time.Millisecond  // a sample of the race's writer, one tick of the hub
	Pause    = 2 * time.Second        // between two races, for the host
	Dwell    = 800 * time.Millisecond // a race's least time in line: every tab sees it join before it leaves
	Accounts = 10                     // accounts of each producer, as in lith's writer benchmark
	Waiting  = 6                      // races in line at most
	Shown    = 8                      // races in the table of results
	Kept     = 100                    // races the sweep keeps
	Charted  = 12                     // races whose samples the sweep keeps
	Stale    = 15 * time.Minute       // a race in line this long is dropped
	// A session starts a race once in ten seconds, after two at once; it
	// has one in line at most anyway.
	startEvery = 10 * time.Second
	startBurst = 2
)

// The topics. Topic is touched when a race starts or ends, LineTopic
// when one joins or leaves the line, LiveTopic by every sample of the race
// on the track, ResultsTopic when a race ends. The table of results
// animates its frames in a view transition, and such a frame goes out on
// every render, changed or not: its view listens to ResultsTopic alone.
const (
	Topic        = Slug
	LineTopic    = Slug + ":line"
	LiveTopic    = Slug + ":live"
	ResultsTopic = Slug + ":results"
)

// Producers are the choices of producers; Syncs of the race file's
// synchronous mode.
var (
	Producers = []int{1, 50, 200}
	Syncs     = []string{"NORMAL", "FULL"}
)

// Settings are a race's choices.
type Settings struct {
	Producers int    `json:"producers"`
	Sync      string `json:"sync"`
}

// Valid reports whether s is one of the choices.
func (s Settings) Valid() bool {
	return slices.Contains(Producers, s.Producers) && slices.Contains(Syncs, s.Sync)
}

// bench are lith's writer benchmark's commands a second for each choice:
// bench/RESULTS.md, an Intel i5-13500, 2026-10-12, medians of three runs.
var bench = map[Settings]int{
	{1, "FULL"}: 526, {50, "FULL"}: 13_260, {200, "FULL"}: 34_893,
	{1, "NORMAL"}: 13_049, {50, "NORMAL"}: 63_267, {200, "NORMAL"}: 66_688,
}

var (
	//go:embed server.md
	server string
	//go:embed how.md
	how string
	//go:embed writer-race.css
	css string
	//go:embed migrations
	migrationFiles embed.FS
)

// Demo is the writer race as the site lists it.
var Demo = demo.Demo{
	Slug:       Slug,
	Title:      "Writer race",
	Summary:    "Up to 200 producers send commands to one writer for three seconds, and everyone watching sees the batches grow, tick by tick.",
	Art:        art,
	Migrations: mustSub("migrations"),
	Server:     server,
	How:        how,
	CSS:        css,
	Components: []string{"gauge", "meter"},
	Mount:      mount,
	Page:       demo.Part{Render: renderPage, Views: pageViews},
	Embed:      &demo.Embed{Slot: demo.SlotLinear, Part: demo.Part{Render: renderEmbed, Views: embedViews}},
	Sweep:      sweep,
}

func mustSub(dir string) fs.FS {
	f, err := fs.Sub(migrationFiles, dir)
	if err != nil {
		panic(err)
	}
	return f
}
