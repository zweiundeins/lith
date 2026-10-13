// Package demos lists the site's demos. A demo is a package under
// internal/demos/<slug> that exports a demo.Demo, and one line in All.
package demos

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demos/board"
	"github.com/zweiundeins/lith/site/internal/demos/canvas"
	"github.com/zweiundeins/lith/site/internal/demos/guestbook"
	"github.com/zweiundeins/lith/site/internal/demos/life"
	"github.com/zweiundeins/lith/site/internal/demos/stream"
	writerrace "github.com/zweiundeins/lith/site/internal/demos/writer-race"
	"github.com/zweiundeins/lith/site/internal/migrate"
	"github.com/zweiundeins/lith/site/migrations"
)

// All are the site's demos, in the order of their cards.
var All = []demo.Demo{
	guestbook.Demo,
	writerrace.Demo,
	stream.Demo,
	canvas.Demo,
	board.Demo,
	life.Demo,
}

// Check validates every demo, and that no two share a slug or a slot of
// the landing page.
func Check(list []demo.Demo) error {
	var errs []error
	slugs := map[string]bool{}
	slots := map[demo.Slot]string{}
	for i := range list {
		d := &list[i]
		if err := d.Validate(); err != nil {
			errs = append(errs, err)
		}
		if slugs[d.Slug] {
			errs = append(errs, fmt.Errorf("demo %q is listed twice", d.Slug))
		}
		slugs[d.Slug] = true
		if d.Embed != nil {
			if other, taken := slots[d.Embed.Slot]; taken {
				errs = append(errs, fmt.Errorf("demos %q and %q both embed in the %s slot", other, d.Slug, d.Embed.Slot))
			}
			slots[d.Embed.Slot] = d.Slug
		}
	}
	return errors.Join(errs...)
}

// Scopes are the migration scopes of the site with these demos: the
// site's own first, then each demo's in the order of their slugs.
func Scopes(list []demo.Demo) []migrate.Scope {
	scopes := []migrate.Scope{{Name: "site", FS: migrations.FS}}
	sorted := slices.Clone(list)
	slices.SortFunc(sorted, func(a, b demo.Demo) int { return cmp.Compare(a.Slug, b.Slug) })
	for _, d := range sorted {
		scopes = append(scopes, migrate.Scope{Name: d.Slug, FS: d.Migrations})
	}
	return scopes
}
