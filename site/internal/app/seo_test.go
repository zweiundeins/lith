package app_test

import (
	"encoding/json"
	"encoding/xml"
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/githistory"
)

var (
	titleTag    = regexp.MustCompile(`<title>([^<]*)</title>`)
	metaContent = func(attr, name string) *regexp.Regexp {
		return regexp.MustCompile(`<meta ` + attr + `="` + regexp.QuoteMeta(name) + `" content="([^"]*)"`)
	}
	metaDescription = metaContent("name", "description")
	ogTitle         = metaContent("property", "og:title")
	ldJSON          = regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`)
)

// TestSEO reads every page of the sitemap as a search engine would: its
// title, its meta description, and its JSON-LD, one schema.org @graph
// with the WebSite and the page's own nodes. It checks the sitemap's
// lastmod too, where the build has the pages' history.
func TestSEO(t *testing.T) {
	_, base := apptest.Start(t)
	c := client(t)
	_, body := get(t, c, base+"/sitemap.xml")
	var sitemap struct {
		URLs []struct {
			Loc     string `xml:"loc"`
			LastMod string `xml:"lastmod"`
		} `xml:"url"`
	}
	if err := xml.Unmarshal([]byte(body), &sitemap); err != nil {
		t.Fatal(err)
	}
	history, err := githistory.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(sitemap.URLs) < 40 {
		t.Fatalf("%d pages in the sitemap", len(sitemap.URLs))
	}
	for _, u := range sitemap.URLs {
		path := strings.TrimPrefix(u.Loc, base)
		if c, ok := history[path]; ok {
			if u.LastMod != c.Date.UTC().Format(time.RFC3339) {
				t.Errorf("%s: lastmod %q, its last commit is of %s", path, u.LastMod, c.Date)
			}
		} else if u.LastMod != "" {
			t.Errorf("%s: lastmod %q without a commit", path, u.LastMod)
		}
		res, page := get(t, c, u.Loc)
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", path, res.StatusCode)
			continue
		}
		checkHead(t, path, page)
		checkGraph(t, base, path, page, history)
	}
}

func checkHead(t *testing.T, path, page string) {
	t.Helper()
	title := titleTag.FindStringSubmatch(page)
	if title == nil {
		t.Errorf("%s: no <title>", path)
		return
	}
	if n := utf8.RuneCountInString(html.UnescapeString(title[1])); n > 60 {
		t.Errorf("%s: a title of %d characters: %q", path, n, title[1])
	}
	if og := ogTitle.FindStringSubmatch(page); og == nil || og[1] != title[1] {
		t.Errorf("%s: og:title is not the title", path)
	}
	d := metaDescription.FindStringSubmatch(page)
	if d == nil {
		t.Errorf("%s: no meta description", path)
		return
	}
	if n := utf8.RuneCountInString(html.UnescapeString(d[1])); n < 70 || n > 160 {
		t.Errorf("%s: a description of %d characters, not 70 to 160: %q", path, n, d[1])
	}
}

type ldNode = map[string]any

func checkGraph(t *testing.T, base, path, page string, history map[string]githistory.Commit) {
	t.Helper()
	scripts := ldJSON.FindAllStringSubmatch(page, -1)
	if len(scripts) != 1 {
		t.Errorf("%s: %d JSON-LD scripts", path, len(scripts))
		return
	}
	var doc struct {
		Context string   `json:"@context"`
		Graph   []ldNode `json:"@graph"`
	}
	if err := json.Unmarshal([]byte(scripts[0][1]), &doc); err != nil {
		t.Errorf("%s: JSON-LD: %v", path, err)
		return
	}
	if doc.Context != "https://schema.org" {
		t.Errorf("%s: @context %q", path, doc.Context)
	}
	byType := map[string]ldNode{}
	ids := map[string]bool{}
	for _, n := range doc.Graph {
		typ, _ := n["@type"].(string)
		id, _ := n["@id"].(string)
		if typ == "" || id == "" || ids[id] {
			t.Errorf("%s: a node without a @type, without an @id, or with one twice: %v", path, n)
		}
		ids[id] = true
		byType[typ] = n
	}
	want := func(typ string, fields map[string]any) ldNode {
		t.Helper()
		n := byType[typ]
		if n == nil {
			t.Errorf("%s: no %s", path, typ)
			return nil
		}
		for k, v := range fields {
			got := n[k]
			if v == nil {
				if s, ok := got.(string); got == nil || ok && s == "" {
					t.Errorf("%s: %s has no %s", path, typ, k)
				}
				continue
			}
			if b, _ := json.Marshal(got); string(b) != mustJSON(v) {
				t.Errorf("%s: %s's %s is %s, not %s", path, typ, k, b, mustJSON(v))
			}
		}
		return n
	}
	org := map[string]any{"@id": "https://zweiundeins.gmbh/#organization"}
	author := map[string]any{"@id": "https://zweiundeins.gmbh/#mbolli"}
	site := map[string]any{"@id": base + "/#website"}
	want("WebSite", map[string]any{"@id": base + "/#website", "name": "lith", "url": base + "/", "publisher": org})
	switch {
	case path == "/":
		n := want("SoftwareSourceCode", map[string]any{
			"name": "lith", "description": nil, "codeRepository": "https://github.com/zweiundeins/lith",
			"programmingLanguage": "Go", "license": "https://opensource.org/licenses/MIT",
			"author": author, "publisher": org,
		})
		if v, _ := n["version"].(string); !regexp.MustCompile(`^v\d+\.\d+\.\d+$`).MatchString(v) {
			t.Errorf("/: version %q", v)
		}
	case strings.HasPrefix(path, "/docs"):
		n := want("TechArticle", map[string]any{
			"headline": nil, "description": nil, "url": base + path, "image": base + "/og.png",
			"author": author, "publisher": org, "isPartOf": site,
		})
		if h, _ := n["headline"].(string); utf8.RuneCountInString(h) > 110 {
			t.Errorf("%s: a headline over 110 characters", path)
		}
		if c, ok := history[path]; ok {
			if n["dateModified"] != c.Date.UTC().Format(time.RFC3339) {
				t.Errorf("%s: dateModified %v", path, n["dateModified"])
			}
		}
		crumbs := want("BreadcrumbList", nil)
		items, _ := crumbs["itemListElement"].([]any)
		if len(items) == 0 || len(items) > 3 {
			t.Errorf("%s: %d breadcrumbs", path, len(items))
			break
		}
		for i, it := range items {
			item, _ := it.(map[string]any)
			if item["@type"] != "ListItem" || item["position"] != float64(i+1) || item["name"] == "" {
				t.Errorf("%s: breadcrumb %d: %v", path, i+1, item)
			}
		}
		first, _ := items[0].(map[string]any)
		last, _ := items[len(items)-1].(map[string]any)
		if first["item"] != base+"/docs" || last["item"] != base+path {
			t.Errorf("%s: breadcrumbs from %v to %v", path, first["item"], last["item"])
		}
	case strings.HasPrefix(path, "/demos"):
		want("WebPage", map[string]any{"name": nil, "description": nil, "url": base + path, "isPartOf": site})
	default:
		t.Errorf("%s: a page of no known kind", path)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
