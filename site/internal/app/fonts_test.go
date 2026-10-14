package app_test

import (
	"fmt"
	"html"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/web"
)

var (
	unicodeRange = regexp.MustCompile(`unicode-range:\s*([^;]+);`)
	hidden       = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>`)
	tag          = regexp.MustCompile(`<[^>]*>`)
)

// TestFontSubset holds the site's fonts to their subset, fonts/unicodes.txt
// (go tool task fonts): every @font-face's unicode-range is that list, and
// every character a page shows is in it, so new copy cannot fall back to
// another font unnoticed.
func TestFontSubset(t *testing.T) {
	b, err := os.ReadFile("../../fonts/unicodes.txt")
	if err != nil {
		t.Fatal(err)
	}
	var ranges []string
	type span struct{ lo, hi rune }
	var spans []span
	for line := range strings.Lines(string(b)) {
		r, _, _ := strings.Cut(line, "#")
		if r = strings.TrimSpace(r); r == "" {
			continue
		}
		ranges = append(ranges, r)
		lo, hi, ok := strings.Cut(strings.TrimPrefix(r, "U+"), "-")
		if !ok {
			hi = lo
		}
		l, err1 := strconv.ParseUint(lo, 16, 32)
		h, err2 := strconv.ParseUint(hi, 16, 32)
		if err1 != nil || err2 != nil || h < l {
			t.Fatalf("unicodes.txt: %q is not a range", r)
		}
		spans = append(spans, span{rune(l), rune(h)})
	}
	css, err := fs.ReadFile(web.StaticFiles(), "css/tokens.css")
	if err != nil {
		t.Fatal(err)
	}
	faces := unicodeRange.FindAllStringSubmatch(string(css), -1)
	if len(faces) != 2 {
		t.Errorf("%d unicode-ranges in tokens.css, want one per @font-face", len(faces))
	}
	for _, f := range faces {
		if got, want := strings.Join(strings.Fields(f[1]), " "), strings.Join(ranges, ", "); got != want {
			t.Errorf("unicode-range: %s\nfonts/unicodes.txt: %s", got, want)
		}
	}

	in := func(r rune) bool {
		for _, s := range spans {
			if s.lo <= r && r <= s.hi {
				return true
			}
		}
		return false
	}
	_, base := apptest.Start(t)
	c := client(t)
	_, sitemap := get(t, c, base+"/sitemap.xml")
	pages := []string{base + "/nope"}
	for _, m := range regexp.MustCompile(`<loc>([^<]+)</loc>`).FindAllStringSubmatch(sitemap, -1) {
		pages = append(pages, m[1])
	}
	missing := map[rune][]string{}
	for _, u := range pages {
		_, page := get(t, c, u)
		text := html.UnescapeString(tag.ReplaceAllString(hidden.ReplaceAllString(page, ""), " "))
		for _, r := range text {
			if w := missing[r]; r >= ' ' && !in(r) && len(w) < 3 && (len(w) == 0 || w[len(w)-1] != strings.TrimPrefix(u, base)) {
				missing[r] = append(missing[r], strings.TrimPrefix(u, base))
			}
		}
	}
	for r, where := range missing {
		t.Errorf("%q (%s) on %s is not in fonts/unicodes.txt: add it and run go tool task fonts", r, fmt.Sprintf("U+%04X", r), strings.Join(where, ", "))
	}
}
