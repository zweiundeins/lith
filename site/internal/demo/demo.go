// Package demo defines what a demo on lith's site is, and holds the kit
// demos are built with: routes for actions behind rate limits, the answer
// to an action, tab state, patches for the stream, and the sweep helpers
// that keep demo tables small.
//
// A demo is a package under internal/demos/<slug> that exports a Demo, and
// one line in internal/demos' list. The site serves it at /demos/<slug>: a
// page with the demo itself, a note on what the server does, a "How it
// works" panel, and the tab's stream (GET /demos/<slug>/stream), which
// watches the views of Page. A demo may also offer an Embed for a slot on
// the landing page.
package demo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/writer"
)

// Demo is one demo of the site.
type Demo struct {
	// Slug names the demo everywhere: its URL (/demos/<slug>), its actions
	// (/cmd/<slug>/<name>), its migration scope, the prefix of its tables
	// (<slug>_, with - as _), of its topics and view keys (<slug>:), of its
	// view-transition names (<slug>-) and its signals ($_<slug> in
	// camelCase). Lowercase letters, digits and -.
	Slug string
	// Title is the demo's name; Summary says in one line what it shows.
	Title, Summary string
	// Art is a small pixel-art glyph as SVG markup (internal/pixelart), for
	// the demo's card and the head of its page; "" takes the site's.
	Art string
	// Migrations holds the demo's NNN_name.sql files at its root, numbered
	// from 001 within the demo (internal/migrate).
	Migrations fs.FS
	// Server is Markdown: what happens on the server while someone uses the
	// demo, shown beside it. How is Markdown for the "How it works" panel
	// under it, with the code that does it.
	Server, How string
	// CSS is the demo's stylesheet. Its rules sit inside
	// @scope ([data-demo="<slug>"]), and the site adds it to its bundle in
	// the components layer.
	CSS string
	// Mount adds the demo's routes through the kit: its actions, and GET
	// routes under /demos/<slug>/ if it needs any.
	Mount func(k *Kit)
	// Page is the demo on its own page.
	Page Part
	// Embed, if set, is a small live part for a slot on the landing page.
	Embed *Embed
	// Sweep keeps the demo's tables small. The site runs it as a command
	// every few minutes (SweepEvery), with the time of the run.
	Sweep func(tx *writer.Tx, now time.Time) error
}

// Part is a live region of a page. Render draws it into the GET document,
// in the document's read transaction; Views keep it current on the tab's
// stream. Both read the same state, so the stream's first frames redraw what
// the document already shows. The site puts Render's markup in the demo's
// island, <div class="island" data-demo="<slug>">, the root of the demo's
// @scope.
type Part struct {
	Render func(ctx context.Context, tx *sql.Tx, v Viewer) (templ.Component, error)
	// Views are the views the tab's stream watches. Shared views (the same
	// for everyone) carry nothing about the viewer; a tab's own view is
	// keyed by its tab (TabTopic).
	Views func(v Viewer) []live.View
}

// Viewer is who looks at a page: the browser's session (a cookie that
// edge.Sessions sets) and the tab, an id that is new for every document.
type Viewer struct {
	Session string
	Tab     string
}

// Slot is a place on the landing page for an Embed.
type Slot string

// The landing page's slots. Hero is the full-width layer behind the hero
// (the cursor party), the others sit beside the four ideas.
const (
	SlotHero       Slot = "hero"
	SlotLinear     Slot = "linear"
	SlotImmediate  Slot = "immediate"
	SlotTicks      Slot = "ticks"
	SlotHypermedia Slot = "hypermedia"
)

// Slots lists the slots in the order the landing page shows them.
var Slots = []Slot{SlotHero, SlotLinear, SlotImmediate, SlotTicks, SlotHypermedia}

// Embed is a small live version of a demo for a slot on the landing page.
// The site puts its markup in the demo's island, like Page's, so the
// demo's CSS applies; it links to the demo's page itself if it should.
type Embed struct {
	Slot Slot
	Part
}

// SweepEvery is how often the site runs every demo's Sweep.
const SweepEvery = 5 * time.Minute

// reserved are slugs that name the site's own routes, scope or tables.
var reserved = map[string]bool{"site": true, "static": true, "stream": true, "cmd": true, "docs": true, "demos": true, "lith": true}

var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)

// Validate checks what the site relies on: the slug's form, the texts and
// the parts every demo has.
func (d *Demo) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !slugPattern.MatchString(d.Slug) || strings.Contains(d.Slug, "--") {
		fail("slug %q: 2 to 32 lowercase letters, digits and single -, starting with a letter", d.Slug)
	}
	if reserved[d.Slug] {
		fail("slug %q is the site's own", d.Slug)
	}
	if strings.TrimSpace(d.Title) == "" || strings.TrimSpace(d.Summary) == "" {
		fail("a title and a one-line summary are required")
	}
	if strings.Contains(d.Summary, "\n") {
		fail("the summary is one line")
	}
	if strings.TrimSpace(d.Server) == "" || strings.TrimSpace(d.How) == "" {
		fail("Server and How (Markdown) are required")
	}
	if d.Page.Render == nil || d.Page.Views == nil {
		fail("Page needs Render and Views")
	}
	if d.CSS != "" && !strings.Contains(d.CSS, `@scope ([data-demo="`+d.Slug+`"])`) {
		fail(`CSS: put the rules in @scope ([data-demo="%s"])`, d.Slug)
	}
	if e := d.Embed; e != nil {
		if e.Render == nil || e.Views == nil {
			fail("Embed needs Render and Views")
		}
		known := false
		for _, s := range Slots {
			known = known || s == e.Slot
		}
		if !known {
			fail("Embed: unknown slot %q", e.Slot)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("demo %q: %w", d.Slug, err)
	}
	return nil
}

// TablePrefix is the prefix of the demo's tables: its slug with - as _,
// and a _.
func (d *Demo) TablePrefix() string { return strings.ReplaceAll(d.Slug, "-", "_") + "_" }

// SignalName is the top-level signal under which the demo's page keeps its
// own signals: $_ and the slug in camelCase ("cursor-party":
// "_cursorParty"). The leading _ keeps them in the browser: Datastar sends
// no signal that starts with one.
func (d *Demo) SignalName() string {
	var b strings.Builder
	b.WriteByte('_')
	up := false
	for _, c := range d.Slug {
		if c == '-' {
			up = true
			continue
		}
		if up && 'a' <= c && c <= 'z' {
			c -= 'a' - 'A'
		}
		up = false
		b.WriteRune(c)
	}
	return b.String()
}
