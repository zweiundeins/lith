package markdown_test

import (
	"strings"
	"testing"

	"github.com/zweiundeins/lith/site/internal/markdown"
)

func TestRender(t *testing.T) {
	d, err := markdown.Render([]byte("# Title\n\nText with `code`.\n\n## The `writer` package\n\n### A detail\n\n```go title=cmd/app/main.go\nfunc main() { fmt.Println(\"hi\") }\n```\n\n| a | b |\n| --- | --- |\n| 1 | 2 |\n"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "Title" {
		t.Errorf("title %q", d.Title)
	}
	if len(d.Headings) != 2 || d.Headings[0].ID != "the-writer-package" || d.Headings[0].Text != "The writer package" || d.Headings[1].Level != 3 {
		t.Errorf("headings %+v", d.Headings)
	}
	for _, want := range []string{
		`<h2 id="the-writer-package">The <code>writer</code> package<a class="anchor" href="#the-writer-package" aria-label="Link to this section"></a></h2>`,
		`<figure class="code"><figcaption class="code__bar"><span class="code__label">cmd/app/main.go</span>`,
		`<span class="kd">func</span>`,
		`<span class="s">&#34;hi&#34;</span>`,
		`data-on:click=`,
		`<table>`,
	} {
		if !strings.Contains(d.HTML, want) {
			t.Errorf("no %s in\n%s", want, d.HTML)
		}
	}
}

func TestHighlightFallback(t *testing.T) {
	if got := markdown.Highlight("<b>", "no-such-language"); !strings.Contains(got, "&lt;b&gt;") {
		t.Errorf("unknown language: %s", got)
	}
}
