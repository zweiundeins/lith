package docs_test

import (
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/zweiundeins/lith/site/content"
	"github.com/zweiundeins/lith/site/internal/docs"
)

// TestContent loads the site's docs and checks every page: its texts, its
// place in the reading order, and that every link inside the site leads to
// a page (and a heading on it) that exists.
func TestContent(t *testing.T) {
	for _, drafts := range []bool{false, true} {
		d, err := docs.Load(content.Docs, drafts)
		if err != nil {
			t.Fatal(err)
		}
		if d.Home.Path != "/docs" || d.Order[0] != d.Home {
			t.Fatalf("the home page is %s", d.Home.Path)
		}
		for i, p := range d.Order {
			if p.Draft && !drafts {
				t.Errorf("%s: a draft on the public site", p.Path)
			}
			if p.Title == "" || p.Summary == "" || strings.TrimSpace(p.HTML) == "" {
				t.Errorf("%s: empty", p.Path)
			}
			if (i > 0) != (p.Prev != nil) || (i+1 < len(d.Order)) != (p.Next != nil) {
				t.Errorf("%s: previous and next pages are wrong", p.Path)
			}
			if d.Page(p.Path) != p {
				t.Errorf("%s: not found by its path", p.Path)
			}
			for _, bad := range []string{"\u2014", "\u2013"} { // em and en dash
				if strings.Contains(p.HTML, bad) {
					t.Errorf("%s: a long dash in the text", p.Path)
				}
			}
		}
		checkLinks(t, d)
	}
}

var href = regexp.MustCompile(`href="(/[^"#]*)?(#[^"]*)?"`)

func checkLinks(t *testing.T, d *docs.Docs) {
	t.Helper()
	ids := map[string]map[string]bool{}
	for _, p := range d.Order {
		ids[p.Path] = map[string]bool{}
		for _, h := range p.Headings {
			ids[p.Path][h.ID] = true
		}
	}
	for _, p := range d.Order {
		for _, m := range href.FindAllStringSubmatch(p.HTML, -1) {
			path, anchor := m[1], strings.TrimPrefix(m[2], "#")
			if path == "" {
				path = p.Path
			}
			switch {
			case path == "/" || path == "/demos" || strings.HasPrefix(path, "/demos/"):
				continue // the site's own pages, checked by the app's tests
			case !strings.HasPrefix(path, "/docs"):
				t.Errorf("%s links to %s, which is not a page of the site", p.Path, path)
			case ids[path] == nil:
				if d.Page(path) == nil {
					t.Errorf("%s links to %s, which does not exist (or is a draft)", p.Path, path)
				}
			case anchor != "" && !ids[path][anchor]:
				t.Errorf("%s links to %s#%s, which has no such heading", p.Path, path, anchor)
			}
		}
	}
}

func TestFrontMatter(t *testing.T) {
	page := func(fm string) fstest.MapFS {
		return fstest.MapFS{
			"index.md":   {Data: []byte("---\ntitle: Docs\nsummary: All.\n---\n\nText.\n")},
			"a/index.md": {Data: []byte(fm)},
		}
	}
	for name, src := range map[string]string{
		"none":        "Text.\n",
		"unclosed":    "---\ntitle: A\n",
		"no title":    "---\nsummary: S.\n---\n",
		"unknown key": "---\ntitle: A\nsummary: S.\ncolour: red\n---\n",
		"bad order":   "---\ntitle: A\nsummary: S.\norder: first\n---\n",
		"an h1":       "---\ntitle: A\nsummary: S.\n---\n\n# A\n",
	} {
		if _, err := docs.Load(page(src), false); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	d, err := docs.Load(page("---\ntitle: A\nsummary: S.\ndraft: true\n---\n\nText.\n"), false)
	if err != nil || len(d.Sections) != 0 {
		t.Errorf("a draft section on the public site: %v %v", err, d)
	}
}
