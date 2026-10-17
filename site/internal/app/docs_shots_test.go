package app_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
	"github.com/zweiundeins/lith/site/internal/browser"
)

// TestShotsDocs writes screenshots of the docs' sections, the guides, the
// charts, a matrix and the reference among them, on desktop and mobile,
// in both themes; go tool task shots runs it with TestShots. Charts and
// the matrix are checked close up as well, since a picture of the whole
// page shrinks them.
//
//	LITH_SITE_SHOTS=/tmp/shots LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestShotsDocs
func TestShotsDocs(t *testing.T) {
	dir := os.Getenv("LITH_SITE_SHOTS")
	if dir == "" {
		t.Skip("set LITH_SITE_SHOTS to a directory for the screenshots")
	}
	_, base := apptest.Start(t)
	b := browser.Start(t)
	light := "document.cookie = 'lith-theme=light; Path=/'"
	for _, s := range []struct {
		name, path string
		w, h       int
		full       bool
		setup      string
	}{
		{"docs-home", "/docs", 1440, 900, true, ""},
		{"docs-guides", "/docs/guides", 1440, 900, false, light},
		{"docs-guide-commands", "/docs/guides/commands", 1440, 900, false, ""},
		{"docs-guide-pages-light", "/docs/guides/pages", 1440, 900, false, light},
		{"docs-datastar", "/docs/datastar", 1440, 900, false, ""},
		{"docs-performance", "/docs/performance", 1440, 900, false, light},
		{"docs-performance-writer", "/docs/performance/writer", 1440, 900, true, ""},
		{"docs-performance-budget-light", "/docs/performance/budget", 1440, 900, true, light},
		{"docs-reference", "/docs/reference", 1440, 900, false, ""},
		{"docs-reference-live", "/docs/reference/live", 1440, 900, false, light},
		{"docs-review", "/docs/review", 1440, 900, false, ""},
		{"docs-changelog", "/docs/changelog", 1440, 900, false, ""},
		{"mobile-docs-guide", "/docs/guides/views", 390, 844, false, ""},
		{"mobile-docs-chart-light", "/docs/performance/encoders", 390, 844, true, light},
		{"mobile-docs-reference", "/docs/reference/writer", 390, 844, false, ""},
		{"docs-guide-streams-matrix", "/docs/guides/streams", 1440, 900, false, ""},
		{"mobile-docs-guide-streams-matrix-light", "/docs/guides/streams", 390, 844, false, light},
	} {
		p := b.NewPage(t, s.w, s.h)
		if s.full {
			p.Call(t, "Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-reduced-motion", "value": "reduce"}}})
		}
		if s.setup != "" {
			p.Navigate(t, base+"/")
			p.Eval(t, s.setup, nil)
		}
		p.Navigate(t, base+s.path)
		p.Eval(t, "document.fonts.ready.then(() => true)", nil)
		time.Sleep(600 * time.Millisecond)
		p.Screenshot(t, filepath.Join(dir, s.name+".png"), s.full)
		if errs := p.Errors(); len(errs) > 0 {
			t.Errorf("%s: %v", s.name, errs)
		}
		if strings.HasPrefix(s.name, "docs-performance-") || strings.Contains(s.name, "-matrix") {
			var n int
			p.Eval(t, `(() => { const cs = document.querySelectorAll('.chart'); cs.forEach((c, i) => c.id = 'chart-' + i); return cs.length })()`, &n)
			for i := range n {
				p.ShotElement(t, fmt.Sprintf("#chart-%d", i), filepath.Join(dir, fmt.Sprintf("detail-%s-chart-%d.png", s.name, i)), 1.5)
			}
		}
		p.Close(t)
	}
}
