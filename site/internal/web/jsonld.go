package web

import (
	"encoding/json"
	"io/fs"
	"regexp"
	"time"

	"github.com/zweiundeins/lith/site/content"
	"github.com/zweiundeins/lith/site/internal/docs"
)

// The company's nodes, published on its own site: pages here name them by
// their @id only.
const (
	organizationID = "https://zweiundeins.gmbh/#organization"
	authorID       = "https://zweiundeins.gmbh/#mbolli"
)

type node = map[string]any

// graph is a page's JSON-LD: the WebSite and the nodes given, in one
// @graph. json.Marshal escapes <, > and &, so it is safe in a <script>.
func (s *Server) graph(nodes ...node) string {
	g := append([]node{{
		"@type":      "WebSite",
		"@id":        s.cfg.Base + "/#website",
		"url":        s.cfg.Base + "/",
		"name":       "lith",
		"inLanguage": "en",
		"publisher":  node{"@id": organizationID},
	}}, nodes...)
	b, err := json.Marshal(node{"@context": "https://schema.org", "@graph": g})
	if err != nil {
		panic(err) // maps of strings and nodes always marshal
	}
	return string(b)
}

func (s *Server) website() node { return node{"@id": s.cfg.Base + "/#website"} }

// software is lith itself, for the landing page.
func (s *Server) software() node {
	return node{
		"@type":               "SoftwareSourceCode",
		"@id":                 s.cfg.Base + "/#software",
		"name":                "lith",
		"description":         description,
		"url":                 s.cfg.Base + "/",
		"codeRepository":      s.cfg.Repo,
		"programmingLanguage": "Go",
		"license":             "https://opensource.org/licenses/MIT",
		"version":             release(s.cfg.Version),
		"author":              node{"@id": authorID},
		"publisher":           node{"@id": organizationID},
	}
}

// article is a docs page, with its place in the docs as breadcrumbs.
func (s *Server) article(d *docs.Docs, p *docs.Page) []node {
	url := s.cfg.Base + p.Path
	a := node{
		"@type":            "TechArticle",
		"@id":              url + "#article",
		"headline":         p.Headline,
		"description":      p.Description,
		"url":              url,
		"mainEntityOfPage": url,
		"image":            s.cfg.Base + "/og.png",
		"inLanguage":       "en",
		"author":           node{"@id": authorID},
		"publisher":        node{"@id": organizationID},
		"isPartOf":         s.website(),
	}
	if c, ok := s.history[p.Path]; ok {
		a["dateModified"] = c.Date.UTC().Format(time.RFC3339)
	}
	crumbs := []*docs.Page{d.Home}
	if sec := p.Section; sec != nil {
		crumbs = append(crumbs, sec.Index)
		if sec.Index != p {
			crumbs = append(crumbs, p)
		}
	}
	items := make([]node, len(crumbs))
	for i, c := range crumbs {
		name := c.Title
		if c == d.Home {
			name = "Docs"
		}
		items[i] = node{"@type": "ListItem", "position": i + 1, "name": name, "item": s.cfg.Base + c.Path}
	}
	return []node{a, {"@type": "BreadcrumbList", "@id": url + "#breadcrumbs", "itemListElement": items}}
}

// webPage is a page of the demos.
func (s *Server) webPage(path, name, description string) node {
	url := s.cfg.Base + path
	return node{
		"@type":       "WebPage",
		"@id":         url + "#webpage",
		"url":         url,
		"name":        name,
		"description": description,
		"inLanguage":  "en",
		"isPartOf":    s.website(),
		"about":       node{"@id": s.cfg.Base + "/#software"},
	}
}

var (
	tagVersion  = regexp.MustCompile(`^v\d+\.\d+\.\d+`)
	changelogV  = regexp.MustCompile(`(?m)^## (v\d+\.\d+\.\d+)`)
	latestEntry = func() string {
		b, _ := fs.ReadFile(content.Docs, "changelog/index.md")
		if m := changelogV.FindSubmatch(b); m != nil {
			return string(m[1])
		}
		return ""
	}()
)

// release is the release a build is of: its version's tag without what git
// describe adds after it (v0.1.0-4-g1a2b3c4 is v0.1.0), or, for a build
// without a tag, the changelog's newest release.
func release(version string) string {
	if v := tagVersion.FindString(version); v != "" {
		return v
	}
	return latestEntry
}
