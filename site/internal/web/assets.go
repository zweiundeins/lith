package web

import (
	"embed"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"time"

	"github.com/zweiundeins/lith/edge"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/minify"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/pixelart"
	"github.com/zweiundeins/lith/site/internal/starbase"
	"github.com/zweiundeins/lith/site/internal/views"
)

//go:embed static
var staticFiles embed.FS

// StaticFiles are the files under static/, which the site serves under
// /static/ besides the ones it makes (site.css, art/logo.svg).
func StaticFiles() fs.FS {
	files, _ := fs.Sub(staticFiles, "static")
	return files
}

// Stylesheets are the site's CSS files, in the order of the bundle. A new
// file goes here. Demos' stylesheets follow them, each in the components
// layer.
var Stylesheets = []string{
	"css/tokens.css",
	"css/theme.css",
	"css/starbase.css",
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
// what the site makes: the CSS bundle, lith's newest-frame.js and the logo
// as a file. Scripts and stylesheets go out minified, except in development.
func (s *Server) loadStatic(list []demo.Demo) error {
	files, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return err
	}
	var opts []edge.StaticOption
	if !s.cfg.Dev {
		opts = append(opts, edge.Minify(minify.Bytes))
	}
	if s.static, err = edge.NewStatic(files, "/static/", opts...); err != nil {
		return err
	}
	bundle, err := Bundle(files, list, s.static.URL)
	if err != nil {
		return err
	}
	s.static.Add("site.css", bundle)
	plugin, err := fs.ReadFile(live.Scripts, live.NewestFrame)
	if err != nil {
		return err
	}
	s.static.Add("lith/"+live.NewestFrame, plugin)
	s.static.Add("art/logo.svg", []byte(pixelart.Logo()))
	if s.starbase, err = starbase.Load(files); err != nil {
		return err
	}
	for _, d := range list {
		if _, err := s.starbase.Scripts(d.Components...); err != nil {
			return fmt.Errorf("demo %s: %w", d.Slug, err)
		}
	}
	datastar := s.static.URL("vendor/datastar-rocket.js")
	m := map[string]any{"imports": map[string]string{"datastar": datastar}}
	integrity := map[string]string{}
	for _, f := range s.starbase.Lazy() {
		integrity["/static/"+f.Name] = f.Integrity // a bundle imports it by its plain name
	}
	if len(integrity) > 0 {
		m["integrity"] = integrity
	}
	importMap, err := json.Marshal(m) // escapes <, > and &: safe in a <script>
	if err != nil {
		return err
	}
	s.assets = views.Assets{
		CSS:         s.static.URL("site.css"),
		Datastar:    views.Script{URL: datastar, Integrity: s.starbase.Lock.Datastar.Integrity},
		Transitions: s.static.URL("js/transitions.js"),
		NewestFrame: s.static.URL("lith/" + live.NewestFrame),
		Font:        s.static.URL("fonts/pixelify-sans.woff2"),
		Icon:        s.static.URL("art/logo.svg"),
		ImportMap:   string(importMap),
	}
	return nil
}

// siteComponents are the Starbase components every page uses: the menu.
var siteComponents = []string{"drawer"}

// components returns the bundles a page loads for the Starbase components
// it uses besides the site's own.
func (s *Server) components(slugs ...string) []views.Script {
	files, err := s.starbase.Scripts(append(append([]string(nil), siteComponents...), slugs...)...)
	if err != nil {
		s.log.Error("starbase", "err", err) // New checked every demo's
		return nil
	}
	out := make([]views.Script, len(files))
	for i, f := range files {
		out[i] = views.Script{URL: s.static.URL(f.Name), Integrity: f.Integrity}
	}
	return out
}

// Bundle joins the stylesheets into one, with font URLs pointing at their
// hashed names, and appends each demo's stylesheet in the components layer.
// The site's edge.Static minifies it.
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

// robots is robots.txt.
func (s *Server) robots() []byte {
	return fmt.Appendf(nil, "User-agent: *\nAllow: /\nDisallow: /cmd/\n\nSitemap: %s/sitemap.xml\n", s.cfg.Base)
}

// sitemap lists the pages, the landing page, the demos and the docs, each
// with the date of its last commit when the build had the history.
func (s *Server) sitemap() ([]byte, error) {
	type url struct {
		Loc     string `xml:"loc"`
		LastMod string `xml:"lastmod,omitempty"`
	}
	paths := []string{"/", "/demos"}
	for _, d := range s.demos {
		paths = append(paths, "/demos/"+d.Slug)
	}
	for _, p := range s.docs.Order {
		paths = append(paths, p.Path)
	}
	urls := make([]url, len(paths))
	for i, p := range paths {
		urls[i].Loc = s.cfg.Base + p
		if c, ok := s.history[p]; ok {
			urls[i].LastMod = c.Date.UTC().Format(time.RFC3339)
		}
	}
	b, err := xml.MarshalIndent(struct {
		XMLName xml.Name `xml:"urlset"`
		NS      string   `xml:"xmlns,attr"`
		URLs    []url    `xml:"url"`
	}{NS: "http://www.sitemaps.org/schemas/sitemap/0.9", URLs: urls}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), append(b, '\n')...), nil
}
