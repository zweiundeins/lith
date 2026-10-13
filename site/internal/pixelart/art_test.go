package pixelart

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArt(t *testing.T) {
	for name, svg := range map[string]string{"logo": Logo(), "hero": Hero()} {
		if !strings.HasPrefix(svg, "<svg") || !strings.Contains(svg, `class="seam"`) || !strings.Contains(svg, `class="halo"`) {
			t.Errorf("%s: not an SVG with a seam and a halo", name)
		}
	}
	for name, b := range map[string][]byte{"social": SocialImage(), "icon180": Icon(180), "icon32": Icon(32)} {
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "social" && (img.Bounds().Dx() != 1200 || img.Bounds().Dy() != 630) {
			t.Errorf("the social image is %v", img.Bounds())
		}
	}
}

// LITH_ART_DUMP=/some/dir go test -run TestDump ./internal/pixelart writes
// the art to look at.
func TestDump(t *testing.T) {
	dir := os.Getenv("LITH_ART_DUMP")
	if dir == "" {
		t.Skip("set LITH_ART_DUMP to write the art")
	}
	files := map[string][]byte{
		"logo.svg": []byte(Logo()), "hero.svg": []byte(Hero()),
		"social.png": SocialImage(), "icon180.png": Icon(180), "icon32.png": Icon(32),
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
