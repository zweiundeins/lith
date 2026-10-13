package web

import (
	"embed"
	"encoding/xml"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"strings"

	"github.com/zweiundeins/lith/edge"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/pixelart"
	"github.com/zweiundeins/lith/site/internal/views"
)

//go:embed static
var staticFiles embed.FS

// Stylesheets are the site's CSS files, in the order of the bundle. A new
// file goes here. Demos' stylesheets follow them, each in the components
// layer.
var Stylesheets = []string{
	"css/tokens.css",
	"css/theme.css",
	"css/reset.css",
	"css/base.css",
	"css/layout.css",
	"css/components/controls.css",
	"css/components/chrome.css",
	"css/components/landing.css",
	"css/components/cards.css",
	"css/components/docs.css",
	"css/components/prose.css",
	"css/components/code.css",
	"css/components/demo.css",
	"css/transitions.css",
	"css/utilities.css",
}

// The raster art, drawn once.
var (
	iconPNG32  = pixelart.Icon(32)
	iconPNG180 = pixelart.Icon(180)
	socialPNG  = pixelart.SocialImage()
)

var fontURL = regexp.MustCompile(`url\("\.\./fonts/([a-z0-9-]+\.woff2)"\)`)

// loadStatic serves the files under static/ by their hashed names, and adds
// what the site makes: the CSS bundle and the logo as a file.
func (s *Server) loadStatic(list []demo.Demo) error {
	files, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return err
	}
	if s.static, err = edge.NewStatic(files, "/static/"); err != nil {
		return err
	}
	bundle, err := Bundle(files, list, s.static.URL)
	if err != nil {
		return err
	}
	s.static.Add("site.css", bundle)
	s.static.Add("art/logo.svg", []byte(pixelart.Logo()))
	s.assets = views.Assets{
		CSS:         s.static.URL("site.css"),
		Datastar:    s.static.URL("vendor/datastar-rocket.js"),
		Transitions: s.static.URL("js/transitions.js"),
		Font:        s.static.URL("fonts/pixelify-sans.woff2"),
		Icon:        s.static.URL("art/logo.svg"),
	}
	return nil
}

// Bundle joins the stylesheets into one, with font URLs pointing at their
// hashed names, and appends each demo's stylesheet in the components layer.
func Bundle(files fs.FS, list []demo.Demo, url func(string) string) ([]byte, error) {
	var b strings.Builder
	for _, name := range Stylesheets {
		css, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, err
		}
		b.WriteString(fontURL.ReplaceAllStringFunc(string(css), func(m string) string {
			return `url("` + url("fonts/"+fontURL.FindStringSubmatch(m)[1]) + `")`
		}))
		b.WriteByte('\n')
	}
	for _, d := range list {
		if d.CSS == "" {
			continue
		}
		fmt.Fprintf(&b, "/* demo: %s */\n@layer components {\n%s\n}\n", d.Slug, d.CSS)
	}
	return []byte(b.String()), nil
}

func (s *Server) robots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "User-agent: *\nAllow: /\nDisallow: /cmd/\n\nSitemap: %s/sitemap.xml\n", s.cfg.Base)
}

// sitemap lists the pages: the landing page, the docs and the demos.
func (s *Server) sitemap(w http.ResponseWriter, r *http.Request) {
	type loc struct {
		Loc string `xml:"loc"`
	}
	urls := []loc{{s.cfg.Base + "/"}, {s.cfg.Base + "/demos"}}
	for _, d := range s.demos {
		urls = append(urls, loc{s.cfg.Base + "/demos/" + d.Slug})
	}
	for _, p := range s.docs.Order {
		urls = append(urls, loc{s.cfg.Base + p.Path})
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	enc.Encode(struct {
		XMLName xml.Name `xml:"urlset"`
		NS      string   `xml:"xmlns,attr"`
		URLs    []loc    `xml:"url"`
	}{NS: "http://www.sitemaps.org/schemas/sitemap/0.9", URLs: urls})
}
