package app_test

import (
	"crypto/sha512"
	"encoding/base64"
	"io/fs"
	"maps"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/zweiundeins/lith/edge"
	"github.com/zweiundeins/lith/live"

	"github.com/zweiundeins/lith/site/internal/app"
	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/web"
)

var (
	scriptOrStyle = regexp.MustCompile(`(?:href|src)="(/static/([^"]+?)\.[0-9a-f]{12}(\.(?:js|css)))"`)
	integrityAttr = regexp.MustCompile(`src="(/static/[^"]+)" integrity="([^"]+)"`)
)

// TestMinified loads the scripts and stylesheets of a page in production
// and in development. In production the site's own go out minified, and
// the vendored builds byte for byte as their integrity says, lith's
// newest-frame.js among them; in development every file goes out as
// written. With -v it lists the sizes,
// raw and under brotli -11.
func TestMinified(t *testing.T) {
	prod, prodPage := pageAssets(t, false)
	dev, devPage := pageAssets(t, true)
	if !slices.Equal(slices.Sorted(maps.Keys(prod)), slices.Sorted(maps.Keys(dev))) || len(prod) < 4 {
		t.Fatalf("production loads %v, development %v", slices.Sorted(maps.Keys(prod)), slices.Sorted(maps.Keys(dev)))
	}
	for page, assets := range map[string]map[string][]byte{prodPage: prod, devPage: dev} {
		pinned := integrityAttr.FindAllStringSubmatch(page, -1)
		if len(pinned) < 2 {
			t.Errorf("%d scripts with an integrity", len(pinned))
		}
		for _, m := range pinned {
			name := scriptOrStyle.FindStringSubmatch(`src="` + m[1] + `"`)
			sum := sha512.Sum384(assets[name[2]+name[3]])
			if got := "sha384-" + base64.StdEncoding.EncodeToString(sum[:]); got != m[2] {
				t.Errorf("%s: %s, the page says %s", m[1], got, m[2])
			}
		}
	}
	plugin, _ := fs.ReadFile(live.Scripts, live.NewestFrame)
	written := map[string][]byte{"lith/" + live.NewestFrame: plugin}
	var total [4]int
	for _, name := range slices.Sorted(maps.Keys(prod)) {
		p, d := prod[name], dev[name]
		file, err := fs.ReadFile(web.StaticFiles(), name)
		if w, ok := written[name]; ok {
			file, err = w, nil
		}
		switch {
		case strings.HasPrefix(name, "vendor/") || name == "lith/"+live.NewestFrame:
			if string(p) != string(d) || string(p) != string(file) {
				t.Errorf("%s: %d bytes in production, %d in development, %d as written", name, len(p), len(d), len(file))
			}
		case name == "site.css":
			if len(p) >= len(d) || !strings.Contains(string(d), "/*") || strings.Contains(string(p), "/*") {
				t.Errorf("site.css: %d bytes in production, %d in development", len(p), len(d))
			}
		default:
			if err != nil || string(d) != string(file) || len(p) >= len(d) {
				t.Errorf("%s: %d bytes in production, %d in development, %d as written (%v)", name, len(p), len(d), len(file), err)
			}
		}
		n := [4]int{len(d), len(p), edge.BrotliLen(d), edge.BrotliLen(p)}
		for i := range total {
			total[i] += n[i]
		}
		t.Logf("%-50s as written %7d, minified %7d; brotli %6d, %6d", name, n[0], n[1], n[2], n[3])
	}
	t.Logf("%-50s as written %7d, minified %7d; brotli %6d, %6d", "together", total[0], total[1], total[2], total[3])
}

// pageAssets returns the scripts and stylesheets a demo page loads, by
// their plain names, and the page.
func pageAssets(t *testing.T, dev bool) (map[string][]byte, string) {
	t.Helper()
	_, base := apptest.Start(t, func(c *app.Config) { c.Dev = dev })
	c := client(t)
	_, page := get(t, c, base+"/demos/life")
	assets := map[string][]byte{}
	for _, m := range scriptOrStyle.FindAllStringSubmatch(page, -1) {
		res, body := get(t, c, base+m[1])
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", m[1], res.StatusCode)
		}
		assets[path.Clean(m[2]+m[3])] = []byte(body)
	}
	return assets, page
}
