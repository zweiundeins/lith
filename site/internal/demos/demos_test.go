package demos_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zweiundeins/lith/sqlite"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demos"
	"github.com/zweiundeins/lith/site/internal/demos/town"
	"github.com/zweiundeins/lith/site/internal/markdown"
	"github.com/zweiundeins/lith/site/internal/migrate"
)

func TestRegistry(t *testing.T) {
	if err := demos.Check(demos.All); err != nil {
		t.Fatal(err)
	}
}

// TestSchema migrates every demo into one database, as the site does, and
// checks that each table belongs to its scope: site_ and lith_ tables to the
// site and lith, every other to the demo whose prefix it carries.
func TestSchema(t *testing.T) {
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "site.db"), sqlite.Options{
		Migrate: func(ctx context.Context, w *sql.DB) error { return migrate.Run(ctx, w, demos.Scopes(demos.All)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.R.Query(`SELECT name FROM sqlite_master WHERE type IN ('table', 'index') AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		rows.Scan(&name)
		owner := ""
		if strings.HasPrefix(name, "site_") || strings.HasPrefix(name, "lith_") {
			owner = "site"
		}
		for i := range demos.All {
			if strings.HasPrefix(name, demos.All[i].TablePrefix()) {
				owner = demos.All[i].Slug
			}
		}
		if owner == "" {
			t.Errorf("table or index %s has no demo's prefix", name)
		}
	}
}

// TestNotes renders every demo's notes and checks that each listing with a
// file in its title is that file's code, so the notes cannot drift from
// it: whitespace aside, the listing must appear in the file.
func TestNotes(t *testing.T) {
	fence := regexp.MustCompile("(?s)```[a-z]* title=([^\\s]+)\n(.*?)```")
	for _, d := range demos.All {
		for name, src := range map[string]string{"Server": d.Server, "How": d.How} {
			doc, err := markdown.Render([]byte(src))
			if err != nil || doc.Title != "" {
				t.Errorf("%s %s: %v, or a # heading in the notes", d.Slug, name, err)
			}
			for _, m := range fence.FindAllStringSubmatch(src, -1) {
				file, code := m[1], m[2]
				body, err := os.ReadFile(filepath.Join(d.Slug, file))
				if err != nil {
					t.Errorf("%s %s: %v", d.Slug, name, err)
					continue
				}
				if !strings.Contains(squash(string(body)), squash(code)) {
					t.Errorf("%s %s: the listing of %s is not in the file:\n%s", d.Slug, name, file, code)
				}
			}
		}
	}
}

var space = regexp.MustCompile(`\s+`)

func squash(s string) string { return strings.TrimSpace(space.ReplaceAllString(s, " ")) }

func TestCheck(t *testing.T) {
	good := demos.All[0]
	for name, change := range map[string]func(*demo.Demo){
		"bad slug":         func(d *demo.Demo) { d.Slug = "Guest_Book" },
		"reserved slug":    func(d *demo.Demo) { d.Slug = "docs" },
		"no summary":       func(d *demo.Demo) { d.Summary = "" },
		"unscoped CSS":     func(d *demo.Demo) { d.CSS = ".x { color: red }" },
		"no page":          func(d *demo.Demo) { d.Page = demo.Part{} },
		"unknown slot":     func(d *demo.Demo) { d.Embed = &demo.Embed{Slot: "footer", Part: good.Page} },
		"two-line summary": func(d *demo.Demo) { d.Summary = "one\ntwo" },
	} {
		d := good
		change(&d)
		if err := demos.Check([]demo.Demo{d}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := demos.Check([]demo.Demo{good, good}); err == nil {
		t.Error("a demo listed twice was accepted")
	}
	one, two := town.Demo, town.Demo
	two.Slug = "town-two"
	if err := demos.Check([]demo.Demo{one, two}); err == nil || !strings.Contains(err.Error(), "both keep events") {
		t.Errorf("two demos that keep events: %v", err)
	}
	if got := (&demo.Demo{Slug: "cursor-party"}).SignalName(); got != "_cursorParty" {
		t.Errorf("signal name %q", got)
	}
}
