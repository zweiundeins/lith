// Package docs loads the documentation: Markdown files under content/docs,
// one directory per section, rendered once at startup into pages with a
// table of contents, their place in the navigation, and the pages before
// and after them in reading order.
//
// Each file starts with front matter between --- lines:
//
//	---
//	title: Ticks
//	summary: Commits mark views dirty; a tick renders each changed view once.
//	order: 3
//	---
//
// title and summary are required; order sorts a section's pages (and, in a
// section's index.md, the sections); nav is a shorter title for the
// navigation; headline is a longer title for the document's <title> and
// og:title, the title by default; description is the page's meta
// description, the summary by default; draft: true keeps a page out
// unless drafts are shown (-dev).
// A section is a directory with an index.md, the section's first page.
package docs

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/zweiundeins/lith/site/internal/markdown"
)

// Page is one page of the docs.
type Page struct {
	Path  string // URL path: /docs, /docs/concepts, /docs/concepts/ticks
	File  string // the file, relative to the docs directory
	Title string
	Nav   string // the navigation's label
	// Headline is the title for search results and shares, the head's
	// <title> and og:title; Title by default.
	Headline string
	Summary  string
	// Description is the head's meta description; Summary by default.
	Description string
	Order       int
	Draft       bool
	Section     *Section // nil for /docs itself
	HTML        string
	Headings    []markdown.Heading
	// IDs are the ids of all its headings, the table of contents' and
	// the deeper ones.
	IDs []string
	// Prev and Next are the pages before and after this one in reading
	// order, nil at the ends.
	Prev, Next *Page
}

// Section is a directory of pages, led by its index.md.
type Section struct {
	Slug  string
	Index *Page
	Pages []*Page // the section's pages after its index, in order
}

// Title is the section's name: its index page's title.
func (s *Section) Title() string { return s.Index.Title }

// Docs are all pages.
type Docs struct {
	Home     *Page
	Sections []*Section
	// Order lists every page in reading order: the home page, then each
	// section's index and pages.
	Order  []*Page
	byPath map[string]*Page
}

// Page returns the page at a URL path, or nil.
func (d *Docs) Page(urlPath string) *Page {
	return d.byPath[strings.TrimSuffix(urlPath, "/")]
}

// Load reads and renders every page of fsys, the docs directory. With
// drafts, pages marked draft are kept, else they are left out.
func Load(fsys fs.FS, drafts bool) (*Docs, error) {
	d := &Docs{byPath: map[string]*Page{}}
	var errs []error
	read := func(file, urlPath string) *Page {
		p, err := readPage(fsys, file, urlPath)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", file, err))
			return nil
		}
		if p.Draft && !drafts {
			return nil
		}
		return p
	}
	d.Home = read("index.md", "/docs")
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			if e.Name() != "index.md" {
				errs = append(errs, fmt.Errorf("%s: pages belong in a section's directory", e.Name()))
			}
			continue
		}
		slug := e.Name()
		index := read(path.Join(slug, "index.md"), "/docs/"+slug)
		if index == nil {
			continue // a draft section, or an error already noted
		}
		s := &Section{Slug: slug, Index: index}
		index.Section = s
		files, err := fs.ReadDir(fsys, slug)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			name := f.Name()
			if f.IsDir() || name == "index.md" || !strings.HasSuffix(name, ".md") {
				continue
			}
			p := read(path.Join(slug, name), "/docs/"+slug+"/"+strings.TrimSuffix(name, ".md"))
			if p == nil {
				continue
			}
			p.Section = s
			s.Pages = append(s.Pages, p)
		}
		slices.SortStableFunc(s.Pages, func(a, b *Page) int { return cmp.Or(cmp.Compare(a.Order, b.Order), cmp.Compare(a.Path, b.Path)) })
		d.Sections = append(d.Sections, s)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if d.Home == nil {
		return nil, errors.New("docs: index.md is missing")
	}
	slices.SortStableFunc(d.Sections, func(a, b *Section) int {
		return cmp.Or(cmp.Compare(a.Index.Order, b.Index.Order), cmp.Compare(a.Slug, b.Slug))
	})
	d.Order = append(d.Order, d.Home)
	for _, s := range d.Sections {
		d.Order = append(d.Order, s.Index)
		d.Order = append(d.Order, s.Pages...)
	}
	for i, p := range d.Order {
		d.byPath[p.Path] = p
		if i > 0 {
			p.Prev = d.Order[i-1]
		}
		if i+1 < len(d.Order) {
			p.Next = d.Order[i+1]
		}
	}
	return d, nil
}

func readPage(fsys fs.FS, file, urlPath string) (*Page, error) {
	src, err := fs.ReadFile(fsys, file)
	if err != nil {
		return nil, err
	}
	meta, body, err := frontMatter(src)
	if err != nil {
		return nil, err
	}
	p := &Page{Path: urlPath, File: file}
	for k, v := range meta {
		switch k {
		case "title":
			p.Title = v
		case "summary":
			p.Summary = v
		case "nav":
			p.Nav = v
		case "headline":
			p.Headline = v
		case "description":
			p.Description = v
		case "order":
			if p.Order, err = strconv.Atoi(v); err != nil {
				return nil, fmt.Errorf("order %q is not a number", v)
			}
		case "draft":
			p.Draft = v == "true"
		default:
			return nil, fmt.Errorf("unknown front matter key %q", k)
		}
	}
	if p.Title == "" || p.Summary == "" {
		return nil, errors.New("front matter needs a title and a summary")
	}
	if p.Nav == "" {
		p.Nav = p.Title
	}
	if p.Headline == "" {
		p.Headline = p.Title
	}
	if p.Description == "" {
		p.Description = p.Summary
	}
	doc, err := markdown.Render(body)
	if err != nil {
		return nil, err
	}
	if doc.Title != "" {
		return nil, errors.New("the page's title comes from its front matter: leave out the # heading")
	}
	p.HTML, p.Headings, p.IDs = doc.HTML, doc.Headings, doc.IDs
	return p, nil
}

// frontMatter splits src into its front matter, as key: value lines between
// two --- lines, and the Markdown after it.
func frontMatter(src []byte) (map[string]string, []byte, error) {
	src = bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	rest, ok := bytes.CutPrefix(src, []byte("---\n"))
	if !ok {
		return nil, nil, errors.New("no front matter: start the file with a --- line")
	}
	head, body, ok := bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		return nil, nil, errors.New("the front matter has no closing --- line")
	}
	meta := map[string]string{}
	for line := range strings.Lines(string(head)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return nil, nil, fmt.Errorf("front matter line %q is not key: value", line)
		}
		meta[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return meta, body, nil
}
