// Package markdown renders the site's Markdown: the docs pages and the
// demos' notes. GitHub's flavour (tables, task lists, autolinks), an id on
// every heading with a link to it ({#id} after a heading sets its own),
// code blocks highlighted by chroma into classes that the stylesheet
// colours with the semantic code tokens (--lith-code-*), so one block
// reads in both themes, and charts drawn as SVG from ```chart blocks.
//
// A code block's info string may carry title=<label> for its caption, or
// source=<path>: a file of lith's repository that holds the listing, which
// the caption names and links (the docs' tests check that it does).
package markdown

import (
	"bytes"
	"fmt"
	"html"
	"strings"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Heading is a heading of a rendered document, for its table of contents.
type Heading struct {
	Level int
	ID    string
	Text  string
}

// Doc is a rendered document.
type Doc struct {
	HTML string
	// Title is the text of the first level-1 heading, if there is one.
	Title string
	// Headings are the level-2 and level-3 headings, in order.
	Headings []Heading
	// IDs are the ids of every heading, at any level.
	IDs []string
}

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID(), parser.WithAttribute()),
	goldmark.WithRendererOptions(
		gmhtml.WithUnsafe(), // the content is the site's own, reviewed like code
		renderer.WithNodeRenderers(
			util.Prioritized(&blockRenderer{}, 100),
		),
	),
)

// Render renders Markdown source.
func Render(src []byte) (Doc, error) {
	root := md.Parser().Parse(text.NewReader(src))
	var d Doc
	err := ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		h, ok := n.(*ast.Heading)
		if !entering || !ok {
			return ast.WalkContinue, nil
		}
		t := plain(h, src)
		id := ""
		if v, ok := h.AttributeString("id"); ok {
			if b, ok := v.([]byte); ok {
				id = string(b)
			}
		}
		if id != "" {
			d.IDs = append(d.IDs, id)
		}
		switch {
		case h.Level == 1 && d.Title == "":
			d.Title = t
		case h.Level == 2 || h.Level == 3:
			d.Headings = append(d.Headings, Heading{Level: h.Level, ID: id, Text: t})
		}
		return ast.WalkSkipChildren, nil
	})
	if err != nil {
		return Doc{}, err
	}
	var b bytes.Buffer
	if err := md.Renderer().Render(&b, src, root); err != nil {
		return Doc{}, err
	}
	d.HTML = b.String()
	return d, nil
}

// plain is the text of an inline subtree, without its markup.
func plain(n ast.Node, src []byte) string {
	var b strings.Builder
	ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := c.(type) {
		case *ast.Text:
			b.Write(t.Segment.Value(src))
			if t.SoftLineBreak() || t.HardLineBreak() {
				b.WriteByte(' ')
			}
		case *ast.String:
			b.Write(t.Value)
		}
		return ast.WalkContinue, nil
	})
	return strings.TrimSpace(b.String())
}

// blockRenderer renders headings with a link to themselves, and code
// blocks through chroma.
type blockRenderer struct{}

func (r *blockRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindHeading, r.heading)
	reg.Register(ast.KindFencedCodeBlock, r.code)
	reg.Register(ast.KindCodeBlock, r.code)
}

func (r *blockRenderer) heading(w util.BufWriter, src []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	n := node.(*ast.Heading)
	id := ""
	if v, ok := n.AttributeString("id"); ok {
		if b, ok := v.([]byte); ok {
			id = string(b)
		}
	}
	if entering {
		fmt.Fprintf(w, "<h%d", n.Level)
		if id != "" {
			fmt.Fprintf(w, ` id="%s"`, html.EscapeString(id))
		}
		w.WriteByte('>')
		return ast.WalkContinue, nil
	}
	if id != "" && n.Level > 1 {
		fmt.Fprintf(w, `<a class="anchor" href="#%s" aria-label="Link to this section"></a>`, html.EscapeString(id))
	}
	fmt.Fprintf(w, "</h%d>\n", n.Level)
	return ast.WalkContinue, nil
}

// copyCode copies the block's code and marks the button for a moment.
const copyCode = `navigator.clipboard.writeText(el.closest('figure').querySelector('code').innerText).then(() => { el.dataset.copied = ''; setTimeout(() => delete el.dataset.copied, 1600) })`

func (r *blockRenderer) code(w util.BufWriter, src []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	lang, title, source := "", "", ""
	if n, ok := node.(*ast.FencedCodeBlock); ok && n.Info != nil {
		lang, title, source = Info(string(n.Info.Segment.Value(src)))
	}
	var code strings.Builder
	lines := node.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		code.Write(seg.Value(src))
	}
	if lang == "chart" {
		c, err := ParseChart(code.String())
		if err != nil {
			return ast.WalkStop, err
		}
		w.WriteString(c.HTML())
		return ast.WalkSkipChildren, nil
	}
	if source != "" {
		w.WriteString(codeBlock(code.String(), lang, `<a href="`+html.EscapeString(SourceURL(source))+`">`+html.EscapeString(source)+`</a>`))
		return ast.WalkSkipChildren, nil
	}
	w.WriteString(CodeBlock(code.String(), lang, title))
	return ast.WalkSkipChildren, nil
}

// Info splits a fenced block's info string into its language and the
// values of title= and source=.
func Info(info string) (lang, title, source string) {
	fields := strings.Fields(info)
	if len(fields) > 0 {
		lang = fields[0]
	}
	for _, f := range fields[min(1, len(fields)):] {
		if t, ok := strings.CutPrefix(f, "title="); ok {
			title = t
		}
		if s, ok := strings.CutPrefix(f, "source="); ok {
			source = s
		}
	}
	return lang, title, source
}

// SourceURL is the address of a file of lith's repository on GitHub, by
// its path from the repository's root.
func SourceURL(path string) string {
	return "https://github.com/zweiundeins/lith/blob/main/" + path
}

// CodeBlock is a highlighted block of code in a figure, captioned with
// title, or else the language, and a button that copies the code.
func CodeBlock(code, lang, title string) string {
	label := title
	if label == "" {
		label = langName(lang)
	}
	return codeBlock(code, lang, html.EscapeString(label))
}

// codeBlock is CodeBlock with a caption of HTML.
func codeBlock(code, lang, label string) string {
	var b strings.Builder
	b.WriteString(`<figure class="code">`)
	b.WriteString(`<figcaption class="code__bar"><span class="code__label">`)
	b.WriteString(label)
	b.WriteString(`</span><button class="code__copy" type="button" data-on:click="`)
	b.WriteString(html.EscapeString(copyCode))
	b.WriteString(`"><span class="code__copy-idle">Copy</span><span class="code__copy-done" aria-live="polite">Copied</span></button></figcaption>`)
	b.WriteString(Highlight(code, lang))
	b.WriteString("</figure>\n")
	return b.String()
}

func langName(lang string) string {
	switch lang {
	case "", "text", "plain":
		return "text"
	case "sh", "bash", "shell", "console":
		return "shell"
	case "go":
		return "Go"
	case "html":
		return "HTML"
	case "sql":
		return "SQL"
	case "css":
		return "CSS"
	case "js", "javascript":
		return "JavaScript"
	case "templ":
		return "templ"
	}
	return lang
}

var formatter = chromahtml.New(chromahtml.WithClasses(true), chromahtml.PreventSurroundingPre(false))

// Highlight returns code as a highlighted <pre class="chroma"> block. Its
// spans carry chroma's short class names (k for a keyword, s for a
// string); the stylesheet maps them to the --lith-code-* tokens.
func Highlight(code, lang string) string {
	if lang == "templ" {
		lang = "go" // close enough: Go with HTML in it
	}
	lexer := lexers.Get(lang)
	if lexer == nil {
		lexer = lexers.Fallback
	}
	it, err := chroma.Coalesce(lexer).Tokenise(nil, code)
	if err == nil {
		var b strings.Builder
		if err = formatter.Format(&b, styles.Fallback, it); err == nil {
			return b.String()
		}
	}
	return `<pre class="chroma"><code>` + html.EscapeString(code) + `</code></pre>`
}
