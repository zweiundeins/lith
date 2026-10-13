package web

import (
	"io/fs"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestContrast checks that the semantic tokens keep WCAG AA in both
// themes: 4.5:1 for text on every surface it sits on, 3:1 for marks that
// are not text. It reads the tokens from the stylesheets, so a change of a
// colour that breaks a pair fails here.
func TestContrast(t *testing.T) {
	files, err := fs.Sub(staticFiles, "static/css")
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{}
	for _, name := range []string{"tokens.css", "theme.css"} {
		b, err := fs.ReadFile(files, name)
		if err != nil {
			t.Fatal(err)
		}
		// Only the first definition of a token counts: theme.css's :root
		// block comes before the blocks that only set color-scheme.
		for _, m := range declaration.FindAllStringSubmatch(string(b), -1) {
			if _, seen := vars[m[1]]; !seen {
				vars[m[1]] = strings.TrimSpace(m[2])
			}
		}
	}
	surfaces := []string{"bg", "surface-1", "surface-2", "surface-inset"}
	type pair struct {
		fg  string
		bgs []string
		min float64
	}
	pairs := []pair{
		{"text-1", surfaces, 4.5},
		{"text-2", surfaces, 4.5},
		{"text-muted", surfaces, 4.5},
		{"accent-text", surfaces, 4.5},
		{"danger", surfaces, 4.5},
		{"ok", surfaces, 3},
		{"focus", surfaces, 3},
		{"on-accent", []string{"accent", "accent-strong"}, 4.5},
		{"code-text", []string{"code-bg"}, 4.5},
		{"code-keyword", []string{"code-bg"}, 4.5},
		{"code-function", []string{"code-bg"}, 4.5},
		{"code-tag", []string{"code-bg"}, 4.5},
		{"code-string", []string{"code-bg"}, 4.5},
		{"code-number", []string{"code-bg"}, 4.5},
		{"code-comment", []string{"code-bg"}, 4.5},
	}
	for theme, side := range map[string]int{"light": 0, "dark": 1} {
		for _, p := range pairs {
			fg := resolve(t, vars, "--lith-"+p.fg, side)
			for _, b := range p.bgs {
				bg := resolve(t, vars, "--lith-"+b, side)
				if c := contrast(fg, bg); c < p.min {
					t.Errorf("%s theme: %s on %s is %.2f:1, want %.1f:1", theme, p.fg, b, c, p.min)
				}
			}
		}
	}
}

var (
	declaration = regexp.MustCompile(`(--lith-[a-z0-9-]+):\s*([^;]+);`)
	lightDark   = regexp.MustCompile(`^light-dark\((.+)\)$`)
	varRef      = regexp.MustCompile(`^var\((--lith-[a-z0-9-]+)\)$`)
	oklch       = regexp.MustCompile(`^oklch\(([\d.]+)%\s+([\d.]+)\s+([\d.]+)\)$`)
)

// resolve follows a token to its colour, in the light (side 0) or the dark
// (side 1) theme.
func resolve(t *testing.T, vars map[string]string, name string, side int) [3]float64 {
	t.Helper()
	v, ok := vars[name]
	if !ok {
		t.Fatalf("no token %s", name)
	}
	for range 10 {
		if m := lightDark.FindStringSubmatch(v); m != nil {
			v = strings.TrimSpace(splitTop(m[1])[side])
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

// splitTop splits light-dark's two arguments at the comma outside
// parentheses.
func splitTop(s string) []string {
	depth := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				return []string{s[:i], s[i+1:]}
			}
		}
	}
	return []string{s, s}
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
