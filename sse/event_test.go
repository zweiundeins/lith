package sse

import (
	"bytes"
	"slices"
	"testing"
)

func TestPatchElements(t *testing.T) {
	for _, c := range []struct {
		name string
		html string
		p    []Patch
		want string
	}{
		{"by id", `<p id="a">hi</p>`, nil,
			"event: datastar-patch-elements\ndata: elements <p id=\"a\">hi</p>\n\n"},
		{"lines", "<ul id=\"l\">\n<li>1</li>\n</ul>", nil,
			"event: datastar-patch-elements\ndata: elements <ul id=\"l\">\ndata: elements <li>1</li>\ndata: elements </ul>\n\n"},
		{"outer is the default", `<p id="a"></p>`, []Patch{{Mode: Outer}},
			"event: datastar-patch-elements\ndata: elements <p id=\"a\"></p>\n\n"},
		{"selector and mode", `<li>3</li>`, []Patch{{Selector: "#list", Mode: Append}},
			"event: datastar-patch-elements\ndata: selector #list\ndata: mode append\ndata: elements <li>3</li>\n\n"},
		{"remove", ``, []Patch{{Selector: "#toast", Mode: Remove}},
			"event: datastar-patch-elements\ndata: selector #toast\ndata: mode remove\n\n"},
		{"namespace", `<circle id="c" r="2"/>`, []Patch{{Namespace: SVG}},
			"event: datastar-patch-elements\ndata: namespace svg\ndata: elements <circle id=\"c\" r=\"2\"/>\n\n"},
		{"html namespace is the default", `<p id="a"></p>`, []Patch{{Namespace: "html"}},
			"event: datastar-patch-elements\ndata: elements <p id=\"a\"></p>\n\n"},
		{"view transition", `<main id="m"></main>`, []Patch{{Selector: "main", Mode: Inner, ViewTransition: true, ViewTransitionSelector: "#m"}},
			"event: datastar-patch-elements\ndata: selector main\ndata: mode inner\ndata: useViewTransition true\ndata: viewTransitionSelector #m\ndata: elements <main id=\"m\"></main>\n\n"},
		{"transition selector needs a transition", `<p id="a"></p>`, []Patch{{ViewTransitionSelector: "#m"}},
			"event: datastar-patch-elements\ndata: elements <p id=\"a\"></p>\n\n"},
		{"carriage returns end lines", "<p id=\"a\">x\r\ny\rz</p>", nil,
			"event: datastar-patch-elements\ndata: elements <p id=\"a\">x\ndata: elements y\ndata: elements z</p>\n\n"},
		{"trailing newline", "<p id=\"a\"></p>\n", nil,
			"event: datastar-patch-elements\ndata: elements <p id=\"a\"></p>\ndata: elements \n\n"},
	} {
		if got := string(PatchElements([]byte(c.html), c.p...)); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestPatchSignals(t *testing.T) {
	if got, want := string(PatchSignals([]byte(`{"a":1}`), false)), "event: datastar-patch-signals\ndata: signals {\"a\":1}\n\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := string(PatchSignals([]byte("{\n\"a\": 1\n}"), true)), "event: datastar-patch-signals\ndata: onlyIfMissing true\ndata: signals {\ndata: signals \"a\": 1\ndata: signals }\n\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestComment(t *testing.T) {
	for in, want := range map[string]string{
		"":         ":\n\n",
		"ping":     ": ping\n\n",
		"a\nb":     ": a\n: b\n\n",
		"a\r\n\nb": ": a\n:\n: b\n\n",
	} {
		if got := string(Comment(in)); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestFormatSizesExactly(t *testing.T) {
	for _, b := range [][]byte{
		PatchElements([]byte("a\nb\r\nc"), Patch{Selector: "x", Mode: Inner, Namespace: MathML, ViewTransition: true, ViewTransitionSelector: "y"}),
		PatchSignals([]byte("{}\n"), true),
		PatchElements(nil),
	} {
		if len(b) != cap(b) {
			t.Errorf("%q: len %d, cap %d", b, len(b), cap(b))
		}
	}
}

func TestReconnect(t *testing.T) {
	want := "event: datastar-patch-elements\ndata: selector body\ndata: mode append\n" +
		`data: elements <div data-init="el.remove(); document.getElementById(&#34;stream&#34;)?.replaceWith(document.getElementById(&#34;stream&#34;).cloneNode(true))"></div>` + "\n\n"
	if got := string(Reconnect("stream")); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// TestLines splits as an event's data lines need, with or without CRs.
func TestLines(t *testing.T) {
	for in, want := range map[string][]string{
		"":              {""},
		"a":             {"a"},
		"a\n":           {"a", ""},
		"a\nb":          {"a", "b"},
		"\n\n":          {"", "", ""},
		"a\r\nb\rc\nd":  {"a", "b", "c", "d"},
		"a\r":           {"a", ""},
		"<p>x</p>\n<p>": {"<p>x</p>", "<p>"},
	} {
		var got []string
		for l := range lines([]byte(in)) {
			got = append(got, string(l))
		}
		if !slices.Equal(got, want) {
			t.Errorf("lines(%q) = %q, want %q", in, got, want)
		}
	}
}

// BenchmarkPatchElementsLarge is a 230 KB page in a patch, as a data
// table's frame.
func BenchmarkPatchElementsLarge(b *testing.B) {
	row := []byte(`<tr><td class="num">1,234</td><td>Alpha Centauri</td><td>G2V</td><td class="num">4.37</td></tr>`)
	page := bytes.Repeat(row, 230<<10/len(row))
	page = append(page, '\n')
	b.SetBytes(int64(len(page)))
	b.ReportAllocs()
	for b.Loop() {
		PatchElements(page)
	}
}

// A Patch option with a line break would end its data line and inject
// another, or a new event: PatchElements refuses it.
func TestPatchOptionsAreSingleLines(t *testing.T) {
	for _, p := range []Patch{
		{Selector: "#a\ndata: elements <p>x</p>"},
		{Selector: "#a\r\n\r\nevent: datastar-patch-signals"},
		{ViewTransition: true, ViewTransitionSelector: "#a\rb"},
		{Mode: "inner\n"},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("PatchElements took %+v", p)
				}
			}()
			PatchElements([]byte(`<p id="a"></p>`), p)
		}()
	}
	PatchElements([]byte(`<p id="a"></p>`), Patch{Selector: "#a", Mode: Inner, ViewTransition: true, ViewTransitionSelector: "#a"})
}
