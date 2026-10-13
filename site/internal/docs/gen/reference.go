package gen

import (
	"bytes"
	"cmp"
	"fmt"
	"go/ast"
	"go/build"
	"go/doc"
	"go/doc/comment"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// refDir is the reference's directory, relative to the root.
const refDir = Docs + "/reference"

// Packages are the packages of the reference, by their directory under
// the root, in the order of its navigation.
var Packages = []string{"sqlite", "writer", "live", "sse", "edge", "tenant", "dbmetrics", "loadtest", "backup", "prom", "dbmetrics/prom"}

// Slug is a package's page name: dbmetrics/prom is dbmetrics-prom.
func Slug(dir string) string { return strings.ReplaceAll(dir, "/", "-") }

// Reference returns the reference's pages: an index, and a page for each
// of Packages.
func Reference(root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	var rows []string
	for i, dir := range Packages {
		p, err := load(root, dir)
		if err != nil {
			return nil, err
		}
		files[refDir+"/"+Slug(dir)+".md"] = p.page(i + 1)
		rows = append(rows, fmt.Sprintf("| [%s](/docs/reference/%s) | %s |", dir, Slug(dir), escapeCell(p.synopsis())))
	}
	files[refDir+"/index.md"] = index(rows)
	return files, nil
}

func escapeCell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

func index(rows []string) []byte {
	var b bytes.Buffer
	b.WriteString(`---
title: Package reference
nav: Packages
summary: Every exported name of lith's packages, with its signature and its doc comment.
order: 6
---

These pages are generated from the Go doc comments of lith's packages, at the commit this site was built from: ` + "`go generate ./content`" + ` in ` + "`site/`" + ` writes them, and ` + "`TestUpToDate`" + ` fails when they differ from the code. To change a page, change the doc comment and generate again.

| Package | What it does |
| --- | --- |
`)
	for _, r := range rows {
		b.WriteString(r + "\n")
	}
	b.WriteString(`
` + "`backup`, `prom` and `dbmetrics/prom`" + ` are modules of their own, so that an app without them never pulls in their dependencies: Litestream and the AWS SDK for ` + "`backup`" + `, the Prometheus client for the other two. The rest are packages of the module ` + "`" + Module + "`" + `, imported one by one.
`)
	return b.Bytes()
}

// pkg is a package read for its page.
type pkg struct {
	dir    string // under the root
	module bool   // a module of its own
	fset   *token.FileSet
	doc    *doc.Package
}

// load parses a package's files as Linux on amd64 builds them, its tests
// included for their examples.
func load(root, dir string) (*pkg, error) {
	abs := filepath.Join(root, filepath.FromSlash(dir))
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	ctx := build.Context{GOOS: "linux", GOARCH: "amd64", Compiler: "gc", ReleaseTags: build.Default.ReleaseTags}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		ok, err := ctx.MatchFile(abs, e.Name())
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(abs, e.Name()), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	d, err := doc.NewFromFiles(fset, files, Module+"/"+dir)
	if err != nil {
		return nil, fmt.Errorf("gen: %s: %w", dir, err)
	}
	_, err = os.Stat(filepath.Join(abs, "go.mod"))
	return &pkg{dir: dir, module: err == nil, fset: fset, doc: d}, nil
}

// synopsis is the first sentence of the package's doc without its
// "Package x is": "The single writer of a SQLite database."
func (p *pkg) synopsis() string {
	s := strings.TrimPrefix(p.doc.Synopsis(p.doc.Doc), "Package "+p.doc.Name+" ")
	s = strings.TrimPrefix(s, "is ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// withoutSynopsis is the package's doc without its first sentence, which
// the page's summary shows already. It walks the doc and the sentence
// together, a run of whitespace in the doc matching one space of the
// sentence, so the doc's own line breaks (code, lists) stay as they are.
func (p *pkg) withoutSynopsis() string {
	doc := strings.TrimSpace(p.doc.Doc)
	first := p.doc.Synopsis(doc)
	space := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' }
	i, j := 0, 0
	for i < len(doc) && j < len(first) {
		switch {
		case space(doc[i]) && first[j] == ' ':
			for i < len(doc) && space(doc[i]) {
				i++
			}
			j++
		case doc[i] == first[j]:
			i, j = i+1, j+1
		default:
			return doc
		}
	}
	if j < len(first) {
		return doc
	}
	return strings.TrimLeft(doc[i:], " \t\n")
}

// page renders the package's page: blocks of Markdown, one blank line
// between each two.
func (p *pkg) page(order int) []byte {
	var b blocks
	b.add(fmt.Sprintf("---\ntitle: %s\nsummary: %s\norder: %d\n---", p.dir, p.synopsis(), order))
	note := fmt.Sprintf("Import path `%s`", p.doc.ImportPath)
	if p.module {
		note += fmt.Sprintf(", a module of its own: `go get %s`", p.doc.ImportPath)
	}
	note += fmt.Sprintf(". Generated from the doc comments in [%s/](https://github.com/zweiundeins/lith/tree/main/%s).", p.dir, p.dir)
	b.add(note)
	p.text(&b, p.withoutSynopsis())
	p.examples(&b, p.doc.Examples, "###", "")
	if len(p.doc.Consts) > 0 {
		b.add("## Constants")
		for _, v := range p.doc.Consts {
			p.value(&b, v)
		}
	}
	if len(p.doc.Vars) > 0 {
		b.add("## Variables")
		for _, v := range p.doc.Vars {
			p.value(&b, v)
		}
	}
	if len(p.doc.Funcs) > 0 {
		b.add("## Functions")
		for _, f := range p.doc.Funcs {
			p.fn(&b, f, "###")
		}
	}
	if len(p.doc.Types) > 0 {
		b.add("## Types")
		for _, t := range p.doc.Types {
			b.add(fmt.Sprintf("### type %s {#%s}", t.Name, t.Name))
			b.add(fence("go", p.declOnly(t.Decl)))
			p.text(&b, t.Doc)
			p.examples(&b, t.Examples, "####", t.Name)
			for _, v := range t.Consts {
				p.value(&b, v)
			}
			for _, v := range t.Vars {
				p.value(&b, v)
			}
			for _, f := range t.Funcs {
				p.fn(&b, f, "####")
			}
			for _, m := range t.Methods {
				p.fn(&b, m, "####")
			}
		}
	}
	return []byte(strings.Join(b, "\n\n") + "\n")
}

// blocks are a page's blocks of Markdown.
type blocks []string

func (b *blocks) add(s string) { *b = append(*b, strings.TrimRight(s, "\n")) }

// value renders a const or var declaration and its doc.
func (p *pkg) value(b *blocks, v *doc.Value) {
	b.add(fence("go", p.declOnly(v.Decl)))
	p.text(b, v.Doc)
}

// fn renders a function or method under a heading of the given level.
func (p *pkg) fn(b *blocks, f *doc.Func, level string) {
	id, title := f.Name, "func "+f.Name
	if f.Decl.Recv != nil && len(f.Decl.Recv.List) == 1 {
		recv := types.ExprString(f.Decl.Recv.List[0].Type)
		base := strings.TrimPrefix(recv, "*")
		if i := strings.IndexByte(base, '['); i >= 0 {
			base = base[:i]
		}
		id, title = base+"."+f.Name, "func ("+recv+") "+f.Name
	}
	b.add(fmt.Sprintf("%s %s {#%s}", level, title, id))
	decl := *f.Decl
	decl.Doc, decl.Body = nil, nil
	b.add(fence("go", p.print(&decl, nil)))
	p.text(b, f.Doc)
	p.examples(b, f.Examples, level+"#", id)
}

// examples renders a declaration's examples.
func (p *pkg) examples(b *blocks, exs []*doc.Example, level, of string) {
	for _, ex := range exs {
		title, id := "Example", "example"
		if of != "" {
			id += "-" + of
		}
		if ex.Suffix != "" {
			title += " (" + ex.Suffix + ")"
			id += "-" + ex.Suffix
		}
		b.add(fmt.Sprintf("%s %s {#%s}", level, title, id))
		p.text(b, ex.Doc)
		comments := slices.DeleteFunc(slices.Clone(ex.Comments), func(c *ast.CommentGroup) bool {
			t := c.Text()
			return strings.HasPrefix(t, "Output:") || strings.HasPrefix(t, "Unordered output:")
		})
		code := p.print(ex.Code, comments)
		if _, ok := ex.Code.(*ast.BlockStmt); ok {
			code = strings.TrimSuffix(strings.TrimPrefix(code, "{\n"), "}")
			var lines []string
			for l := range strings.Lines(code) {
				lines = append(lines, strings.TrimPrefix(l, "\t"))
			}
			code = strings.TrimRight(strings.Join(lines, ""), "\n \t")
		}
		b.add(fence("go", code))
		if ex.Output != "" || ex.EmptyOutput {
			b.add("It prints:")
			b.add(fence("text", ex.Output))
		}
	}
}

// declOnly is a declaration without its doc comment, which the page
// renders as text, printed with the comments of the fields and specs it
// keeps (go/doc dropped the unexported ones, and their comments with
// them).
func (p *pkg) declOnly(d *ast.GenDecl) string {
	g := *d
	g.Doc = nil
	if len(g.Specs) == 1 {
		if ts, ok := g.Specs[0].(*ast.TypeSpec); ok {
			t := *ts
			t.Doc = nil
			g.Specs = []ast.Spec{&t}
		}
	}
	var keep []*ast.CommentGroup
	add := func(c *ast.CommentGroup) {
		if c != nil {
			keep = append(keep, c)
		}
	}
	ast.Inspect(&g, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Field:
			add(x.Doc)
			add(x.Comment)
		case *ast.ValueSpec:
			add(x.Doc)
			add(x.Comment)
		case *ast.TypeSpec:
			add(x.Doc)
			add(x.Comment)
		}
		return true
	})
	slices.SortFunc(keep, func(a, b *ast.CommentGroup) int { return cmp.Compare(a.Pos(), b.Pos()) })
	return p.print(&g, keep)
}

// print formats a node as gofmt does, with the given comments.
func (p *pkg) print(n ast.Node, comments []*ast.CommentGroup) string {
	var buf bytes.Buffer
	cfg := printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}
	var node any = n
	if comments != nil {
		node = &printer.CommentedNode{Node: n, Comments: comments}
	}
	if err := cfg.Fprint(&buf, p.fset, node); err != nil {
		return fmt.Sprintf("// %v", err)
	}
	return buf.String()
}

func fence(lang, code string) string {
	if !strings.HasSuffix(code, "\n") {
		code += "\n"
	}
	return "```" + lang + "\n" + code + "```\n"
}

// text renders a doc comment as Markdown: Go's printer for its paragraphs,
// lists and headings, and a fenced block for its code, marked as Go where
// it parses as Go.
func (p *pkg) text(b *blocks, src string) {
	if strings.TrimSpace(src) == "" {
		return
	}
	d := p.doc.Parser().Parse(src)
	pr := p.doc.Printer()
	pr.HeadingLevel = 4
	for _, blk := range d.Content {
		if c, ok := blk.(*comment.Code); ok {
			lang := "text"
			if isGo(c.Text) {
				lang = "go"
			}
			b.add(fence(lang, c.Text))
			continue
		}
		b.add(string(pr.Markdown(&comment.Doc{Content: []comment.Block{blk}, Links: d.Links})))
	}
}

// isGo reports whether code parses as Go statements or declarations.
func isGo(code string) bool {
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, "", "package p\nfunc _() {\n"+code+"\n}\n", 0); err == nil {
		return true
	}
	_, err := parser.ParseFile(fset, "", "package p\n"+code, 0)
	return err == nil
}
