// Package views renders the site's pages with templ: the shell around every
// page, the landing page, the docs, the demo pages. Demos render their own
// parts in their packages and may use the helpers here (Art, Icon, Count).
package views

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"github.com/zweiundeins/lith/site/internal/pixelart"
)

// Assets are the hashed URLs of the files every page loads.
type Assets struct {
	CSS         string // the stylesheet bundle
	Datastar    Script // the vendored Datastar build
	Transitions string // transitions.js: view transitions and the display menu
	NewestFrame string // lith's newest-frame.js (live.NewestFrame), for pages that stream
	Font        string // Pixelify Sans, preloaded for the display text
	Icon        string // the logo as an SVG file, the favicon
	// ImportMap maps 'datastar' to Datastar's URL for the Starbase
	// components' bundles, with the integrity of the files they load on
	// first use: JSON, safe to put in a <script>.
	ImportMap string
}

// Script is a module a page loads, with its integrity.
type Script struct {
	URL       string
	Integrity string
}

// Shell is the frame around a page.
type Shell struct {
	// Title is the page's own title; the document's is "<Title> · lith",
	// or just "lith" when it is empty (the landing page).
	Title string
	// Headline, when set, takes Title's place in the document's title
	// and og:title: a longer one for search results and shares.
	Headline    string
	Description string
	// Path is the page's canonical path, Base the site's public URL.
	Path, Base string
	// Nav names the header link of the current section: "docs", "demos".
	Nav string
	// Theme is "dark" (the default), "light" or "system"; Style is "pixel"
	// (the default) or "smooth". Both come from cookies, so the page is
	// drawn in them from the first byte.
	Theme, Style string
	Assets       Assets
	Nonce        string
	Version      string
	Repo         string
	// Stream is the URL of the page's stream, without its query; "" for a
	// page that does not stream. The page opens it with the tab id Tab.
	Stream string
	Tab    string
	// Components are the Starbase components' bundles the page loads.
	Components []Script
	// JSONLD is the page's structured data, a schema.org @graph as JSON.
	JSONLD string
	// Docs is the docs' navigation, for the menu on narrow screens; nil
	// outside the docs.
	Docs templ.Component
}

// DocumentTitle is the <title> of the page.
func (s Shell) DocumentTitle() string {
	switch {
	case s.Headline != "":
		return s.Headline + " · lith"
	case s.Title == "":
		return "lith: Linear writes, Immediate mode, Ticks, Hypermedia"
	}
	return s.Title + " · lith"
}

// colorScheme is the <meta name="color-scheme"> of a theme.
func colorScheme(theme string) string {
	switch theme {
	case "light":
		return "light"
	case "system":
		return "light dark"
	}
	return "dark"
}

// StreamID is the id of the element that opens a page's stream, and the
// stream's live.Spec.Reconnect: when the server restarts, the stream's last
// event re-creates the element, and its data-init opens the stream again.
// Its data-indicator keeps $_live true while the stream is open.
const StreamID = "stream"

// openStream opens the tab's stream. On a slow link, measured as the page
// starts or by newest-frame.js once the page falls behind, it sends
// Save-Data: on (edge.SlowLink). The URL stays the same, so a stream opened
// again aborts the old request. retry 'error' keeps trying through a
// restart's 502s and 503s and never gives up; a stream the server ends
// stays closed, except through sse.Reconnect.
func openStream(url string) string {
	return "@get(" + jsString(url+"?tab=") + " + $_tab, {headers: $_slow ? {'Save-Data': 'on'} : {}, retry: 'error', retryMaxCount: Infinity, retryMaxWait: 10000})"
}

// slowLink is true on a slow link, measured once as the page starts. Where
// the browser has navigator.connection (Chromium): data saving, or a link
// it calls 2g or 3g. Elsewhere, from Navigation Timing: the document's
// first byte more than 300 ms after its request, or under 100 KB/s.
const slowLink = `((c, n) => c ? c.saveData || ['slow-2g', '2g', '3g'].includes(c.effectiveType) : !!n && n.transferSize > 0 && (n.responseStart - n.requestStart > 300 || n.transferSize < 100 * (n.responseEnd - n.responseStart)))(navigator.connection, performance.getEntriesByType('navigation')[0])`

// pageSignals are the signals every streaming page starts with: the tab
// id, local to the browser (_), and sent along by the actions that need
// it, and whether the link is slow.
func pageSignals(tab string) string {
	return "{_tab: " + jsString(tab) + ", _slow: " + slowLink + "}"
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return strings.ReplaceAll(string(b), "'", `'`)
}

// jsonLD is the page's structured data.
func jsonLD(s Shell) templ.Component {
	return templ.Raw(`<script type="application/ld+json">` + s.JSONLD + `</script>`)
}

// importMap is the page's import map, with the request's nonce.
func importMap(s Shell) templ.Component {
	return templ.Raw(`<script type="importmap" nonce="` + s.Nonce + `">` + s.Assets.ImportMap + `</script>`)
}

// Art inlines pixel art as decorative SVG, with the given classes. The art's
// own <style>, which animates it as a file, stays out: inlined, the site's
// stylesheet animates it (--lith-seam-glow).
func Art(svg, class string) templ.Component {
	if i := strings.Index(svg, "<style>"); i >= 0 {
		if j := strings.Index(svg[i:], "</style>"); j >= 0 {
			svg = svg[:i] + svg[i+j+len("</style>"):]
		}
	}
	svg = strings.Replace(svg, `class="px-art"`, `class="px-art `+class+`" aria-hidden="true" focusable="false"`, 1)
	return templ.Raw(svg)
}

// logo and hero are rendered once.
var (
	logoSVG = pixelart.Logo()
	heroSVG = pixelart.Hero()
)

// LogoSVG is the small monolith.
func LogoSVG() string { return logoSVG }

// Count says how many of a thing there are: "No entries yet", "1 entry",
// "12 entries".
func Count(n int, one, many string) string {
	switch n {
	case 0:
		return "No " + many + " yet"
	case 1:
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func safe(s string) templ.SafeURL { return templ.SafeURL(s) }

// vtName is a style that gives an element a view-transition name.
func vtName(name string) string { return "view-transition-name: " + name }

func itoa(n int) string { return strconv.Itoa(n) }

func pressed(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
