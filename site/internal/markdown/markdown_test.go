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

func TestSourceAndHeadingIDs(t *testing.T) {
	d, err := markdown.Render([]byte("## func (*Writer) Do {#Writer.Do}\n\n```go source=writer/writer.go\nfunc (w *Writer) Do() {}\n```\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Headings) != 1 || d.Headings[0].ID != "Writer.Do" || d.Headings[0].Text != "func (*Writer) Do" {
		t.Errorf("headings %+v", d.Headings)
	}
	if want := `<span class="code__label"><a href="https://github.com/zweiundeins/lith/blob/main/writer/writer.go">writer/writer.go</a></span>`; !strings.Contains(d.HTML, want) {
		t.Errorf("no %s in\n%s", want, d.HTML)
	}
}

func TestChart(t *testing.T) {
	bars := "```chart\nkind: bars\ntitle: Commands a second\nx: producers\ny: commands a second\nsource: bench/RESULTS.md\n---\nFULL,1,526\nFULL,200,34893\nNORMAL,1,13049\nNORMAL,200,66688\n```\n"
	d, err := markdown.Render([]byte(bars))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<figure class="chart chart--bars">`,
		`<a class="chart__source" href="https://github.com/zweiundeins/lith/blob/main/bench/RESULTS.md">bench/RESULTS.md</a>`,
		`role="img" aria-label="Commands a second"`,
		`>66,688</text>`,
		`<title>NORMAL, producers 200: 66,688 commands a second</title>`,
		`>80K</text>`,
		`<span class="chart__key chart__s2" aria-hidden="true"></span>NORMAL</li>`,
		`<tr><th scope="row">200</th><td>34,893</td><td>66,688</td></tr>`,
	} {
		if !strings.Contains(d.HTML, want) {
			t.Errorf("no %s in\n%s", want, d.HTML)
		}
	}
	lines := "```chart\nkind: lines\ntitle: Heap\nx: open streams\ny: MiB\nsource: bench/budget/simulation.csv\n---\n\"budget, 503\",0,0\n\"budget, 503\",2000,125.5\nnone,0,0\nnone,2000,3312.0\n```\n"
	d, err = markdown.Render([]byte(lines))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<polyline class="chart__line"`, `>3,312.0</text>`, `budget, 503</li>`, `>2,000</text>`} {
		if !strings.Contains(d.HTML, want) {
			t.Errorf("no %s in\n%s", want, d.HTML)
		}
	}
	for name, src := range map[string]string{
		"no separator": "kind: bars\ntitle: T\nsource: s\nA,1,2\n",
		"bad kind":     "kind: pie\ntitle: T\nsource: s\n---\nA,1,2\n",
		"no source":    "kind: bars\ntitle: T\n---\nA,1,2\n",
		"bad number":   "kind: bars\ntitle: T\nsource: s\n---\nA,1,two\n",
		"bad x":        "kind: lines\ntitle: T\nsource: s\n---\nA,one,2\n",
		"no data":      "kind: bars\ntitle: T\nsource: s\n---\n",
		"five series":  "kind: bars\ntitle: T\nsource: s\n---\nA,1,1\nB,1,1\nC,1,1\nD,1,1\nE,1,1\n",
	} {
		if _, err := markdown.Render([]byte("```chart\n" + src + "```\n")); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNumber(t *testing.T) {
	for in, want := range map[string]string{"526": "526", "34893": "34,893", "1234567": "1,234,567", "3312.0": "3,312.0", "0.640": "0.640", "-1500": "-1,500", "2^19": "2^19"} {
		if got := markdown.Number(in); got != want {
			t.Errorf("Number(%q) = %q, want %q", in, got, want)
		}
	}
}
