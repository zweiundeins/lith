package cursorparty

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestContrast checks the party's colours, its own tokens in
// cursor-party.css, against the site's surfaces in both themes, as the
// site's TestContrast checks its tokens: 3:1 for a cursor's arrow and
// trail, which are marks, and 4.5:1 for the name on its tag.
func TestContrast(t *testing.T) {
	vars := map[string]string{}
	for _, name := range []string{"../../web/static/css/tokens.css", "../../web/static/css/theme.css", "cursor-party.css"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range declaration.FindAllStringSubmatch(string(b), -1) {
			if _, seen := vars[m[1]]; !seen {
				vars[m[1]] = strings.TrimSpace(m[2])
			}
		}
	}
	for theme, side := range map[string]int{"light": 0, "dark": 1} {
		for i := range hues {
			hue := resolve(t, vars, "--_h"+strconv.Itoa(i), side)
			for _, s := range []string{"--lith-bg", "--lith-surface-1", "--lith-surface-inset"} {
				if c := contrast(hue, resolve(t, vars, s, side)); c < 3 {
					t.Errorf("%s theme: %s (%s) on %s is %.2f:1, want 3:1", theme, hues[i], "--_h"+strconv.Itoa(i), s, c)
				}
			}
			if c := contrast(resolve(t, vars, "--_on-hue", side), hue); c < 4.5 {
				t.Errorf("%s theme: a name on %s is %.2f:1, want 4.5:1", theme, hues[i], c)
			}
		}
	}
}

var (
	declaration = regexp.MustCompile(`(--[a-z0-9_-]+):\s*([^;{}]+);`)
	lightDark   = regexp.MustCompile(`^light-dark\((.+)\)$`)
	varRef      = regexp.MustCompile(`^var\((--[a-z0-9_-]+)\)$`)
	oklch       = regexp.MustCompile(`^oklch\(([\d.]+)%\s+([\d.]+)\s+([\d.]+)\)$`)
)

// resolve follows a custom property to its colour, in the light (side 0)
// or the dark (side 1) theme.
func resolve(t *testing.T, vars map[string]string, name string, side int) [3]float64 {
	t.Helper()
	v, ok := vars[name]
	if !ok {
		t.Fatalf("no custom property %s", name)
	}
	for range 10 {
		if m := lightDark.FindStringSubmatch(v); m != nil {
			v = strings.TrimSpace(strings.SplitN(m[1], ",", 2)[side])
			continue
		}
		if m := varRef.FindStringSubmatch(v); m != nil {
			if v, ok = vars[m[1]]; !ok {
				t.Fatalf("%s refers to %s, which is not defined", name, m[1])
			}
			continue
		}
		break
	}
	m := oklch.FindStringSubmatch(v)
	if m == nil {
		t.Fatalf("%s is %q: want an opaque oklch(), light-dark() or var()", name, v)
	}
	l, _ := strconv.ParseFloat(m[1], 64)
	c, _ := strconv.ParseFloat(m[2], 64)
	h, _ := strconv.ParseFloat(m[3], 64)
	return srgb(l/100, c, h)
}

// srgb converts OKLCH to linear sRGB, clipped to the gamut.
func srgb(l, c, h float64) [3]float64 {
	a, b := c*math.Cos(h*math.Pi/180), c*math.Sin(h*math.Pi/180)
	l_ := l + 0.3963377774*a + 0.2158037573*b
	m_ := l - 0.1055613458*a - 0.0638541728*b
	s_ := l - 0.0894841775*a - 1.2914855480*b
	L, M, S := l_*l_*l_, m_*m_*m_, s_*s_*s_
	clip := func(x float64) float64 { return math.Min(1, math.Max(0, x)) }
	return [3]float64{
		clip(4.0767416621*L - 3.3077115913*M + 0.2309699292*S),
		clip(-1.2684380046*L + 2.6097574011*M - 0.3413193965*S),
		clip(-0.0041960863*L - 0.7034186147*M + 1.7076147010*S),
	}
}

// contrast is WCAG's contrast ratio of two linear sRGB colours.
func contrast(a, b [3]float64) float64 {
	lum := func(c [3]float64) float64 { return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2] }
	x, y := lum(a), lum(b)
	return (math.Max(x, y) + 0.05) / (math.Min(x, y) + 0.05)
}
