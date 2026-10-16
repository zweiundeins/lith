package docs_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"

	"github.com/zweiundeins/lith/site/content"
	"github.com/zweiundeins/lith/site/internal/docs"
)

// TestSkill holds the docs to lith's skill, skills/lith, which says the
// same things for coding agents in its own words. The docs are written for
// people and not generated from it, so this test checks that nothing the
// skill teaches is missing here: a check, a gotcha, a recipe or a section
// of numbers that the skill gains fails it until the docs have it too.
func TestSkill(t *testing.T) {
	refs := filepath.Join(root(t), "skills", "lith", "references")
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(refs, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	files := pages(t)

	// Every rg command of the skill's checklist is on the review page, as
	// it stands.
	review := squash(files["review/index.md"])
	commands := 0
	for _, span := range codeSpans(read("review.md")) {
		if strings.HasPrefix(span, "rg ") {
			commands++
			if !strings.Contains(review, span) {
				t.Errorf("review.md's %s is not on /docs/review", span)
			}
		}
	}
	if commands == 0 {
		t.Error("no rg commands in review.md")
	}

	// Every piece of code the skill's Datastar notes name is on the
	// Datastar notes page.
	datastar := squash(files["datastar/index.md"])
	for _, span := range codeSpans(read("datastar.md")) {
		if !strings.Contains(datastar, span) {
			t.Errorf("datastar.md's %s is not on /docs/datastar", span)
		}
	}

	// Every recipe and every section of numbers has a page that covers it.
	d, err := docs.Load(content.Docs, false)
	if err != nil {
		t.Fatal(err)
	}
	for file, homes := range map[string]map[string]string{
		"recipes.md": {
			"Start a new project":               "/docs/getting-started",
			"Wire a project":                    "/docs/getting-started/wiring",
			"A command":                         "/docs/guides/commands#a-command",
			"Query code for reads and commands": "/docs/guides/reads#query-code-that-every-reader-shares",
			"A view":                            "/docs/guides/views#a-shared-view",
			"Tab state and preferences":         "/docs/guides/pages#tab-state-in-sqlite",
			"A background job":                  "/docs/guides/migrations-and-jobs#background-jobs",
			"A bulk import":                     "/docs/guides/migrations-and-jobs#bulk-imports",
			"Work a transaction cannot hold":    "/docs/guides/migrations-and-jobs#work-a-transaction-cannot-hold",
			"An event-sourced command":          "/docs/guides/commands#an-event-sourced-command",
			"Projections of events":             "/docs/guides/events#inline-projectors",
			"Backups":                           "/docs/guides/backups",
			"Monitoring on a shared host":       "/docs/guides/monitoring#drop-in-files-for-a-shared-host",
			"Measuring a change":                "/docs/guides/load-testing",
		},
		"performance.md": {
			"Writer":      "/docs/performance/writer",
			"Streams":     "/docs/performance/encoders",
			"Render loop": "/docs/performance/render-loop",
			"Measuring":   "/docs/guides/load-testing",
		},
	} {
		headings := sectionHeadings(read(file))
		for _, h := range headings {
			if homes[h] == "" {
				t.Errorf("%s has a section %q that no docs page covers: add it to the docs, and here", file, h)
			}
		}
		for h, home := range homes {
			if !slices.Contains(headings, h) {
				t.Errorf("%s has no section %q any more", file, h)
			}
			path, anchor, _ := strings.Cut(home, "#")
			p := d.Page(path)
			if p == nil || anchor != "" && !slices.Contains(p.IDs, anchor) {
				t.Errorf("%s's %q: no page %s", file, h, home)
			}
		}
	}
}

// codeSpans returns the text of every code span of a Markdown file, its
// whitespace squashed.
func codeSpans(src string) []string {
	b := []byte(src)
	doc := goldmark.New().Parser().Parse(text.NewReader(b))
	var spans []string
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering || n.Kind() != ast.KindCodeSpan {
			return ast.WalkContinue, nil
		}
		var s strings.Builder
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if t, ok := c.(*ast.Text); ok {
				s.Write(t.Segment.Value(b))
				if t.SoftLineBreak() {
					s.WriteByte(' ')
				}
			}
		}
		spans = append(spans, squash(s.String()))
		return ast.WalkSkipChildren, nil
	})
	return spans
}

// sectionHeadings returns the text of a Markdown file's ## headings.
func sectionHeadings(src string) []string {
	var hs []string
	for line := range strings.Lines(src) {
		if h, ok := strings.CutPrefix(line, "## "); ok {
			hs = append(hs, strings.TrimSpace(h))
		}
	}
	return hs
}
