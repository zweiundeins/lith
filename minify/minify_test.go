package minify_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/css"
	"github.com/tdewolff/parse/v2/js"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/minify"
)

// module has what a minifier can break: imports and exports, strings with
// both quotes and escapes, template literals with and without a tag, a
// regular expression, private fields, optional chaining, a label, lines
// that start with ( and [ without a semicolon before, top-level await.
const module = `// A module.
import { attribute, effect } from 'datastar'
import * as datastar from 'datastar'
import Counter, { named as alias } from './dep.js'
import './side-effect.js'

export { alias as reexported }
export * from './dep.js'
export * as dep from './dep.js'
export const answer = 42

/** html keeps the raw text, as a component's template tag does. */
const html = (strings, ...values) => strings.raw.map((s, i) => s + (i < values.length ? values[i] : '')).join('')

const quotes = 'it\'s "quoted"'
const escapes = "tab\there\nline \u00e9 ü \\ backslash </script>"
const tagged = html` + "`" + `<p class="a  b">
  ${quotes}  </p>\n` + "`" + `
const styles = /* css */ ` + "`" + `
  :host { display: block; }
  .row  >  .cell { color: red; }
` + "`" + `
const untagged = ` + "`" + `line one
line two ${answer} \` + "`" + ` backtick` + "`" + `
const path = String.raw` + "`" + `C:\${'dir'}\file.js` + "`" + `
const pattern = /<(\/?)([a-z][^\s/>]*)/gi
const maybe = globalThis.missing?.deep ?? 'fallback'

export function exported(first, second) {
  const longLocalName = first + second
  return longLocalName
}

export default class Widget {
  #count = 0
  static instances = 0
  bump() {
    Widget.instances++
    return ++this.#count
  }
}

let counter = 0
outer: for (const x of [1, 2, 3]) {
  for (const y of [1, 2]) {
    if (x === 2) continue outer
    counter += x * y
  }
}
const list = [3, 1, 2]
;[...list].sort()
;(() => { counter++ })()

const lazy = await import('./dep.js')

export const result = {
  quotes, escapes, tagged, styles, untagged, path, maybe, counter,
  matched: '<Div id=x></Div>'.match(pattern),
  sum: exported(2, 3),
  bumped: new Widget().bump(),
  fromLazy: lazy.named,
  counter2: new Counter().count,
  datastar: Object.keys(datastar).sort(),
  imported: [typeof attribute, typeof effect],
}
`

// dep.js and the rest that module imports, for node.
var deps = map[string]string{
	"dep.js":                             "export default class Counter { count = 7 }\nexport const named = 'named'\n",
	"side-effect.js":                     "globalThis.sideEffect = true\n",
	"package.json":                       `{"type": "module"}`,
	"node_modules/datastar/package.json": `{"type": "module", "main": "index.js"}`,
	"node_modules/datastar/index.js":     "export const attribute = () => {}, effect = () => {}\n",
}

func TestModule(t *testing.T) {
	out, err := minify.Bytes("text/javascript", []byte(module))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) >= len(module)*9/10 {
		t.Errorf("%d bytes of %d", len(out), len(module))
	}
	written, minified := parseJS(t, []byte(module)), parseJS(t, out)
	if a, b := moduleNames(written), moduleNames(minified); !slices.Equal(a, b) {
		t.Errorf("imports and exports:\n%q\nminified:\n%q", a, b)
	}
	if a, b := taggedText(written), taggedText(minified); len(a) != 2 || !slices.Equal(a, b) {
		t.Errorf("tagged templates' text:\n%q\nminified:\n%q", a, b)
	}
	for _, kept := range []string{`"./side-effect.js"`, `export*as dep from"./dep.js"`, `export default class`} {
		if !bytes.Contains(out, []byte(kept)) {
			t.Errorf("no %s in\n%s", kept, out)
		}
	}
	if bytes.Contains(out, []byte("longLocalName")) {
		t.Error("a local name kept its length")
	}

	// Both run in node to the same result.
	if _, err := exec.LookPath("node"); err != nil {
		t.Log("node is not installed: the minified module was parsed, not run")
		return
	}
	dir := t.TempDir()
	for name, text := range deps {
		write(t, dir, name, text)
	}
	write(t, dir, "as-written.js", module)
	write(t, dir, "minified.js", string(out))
	cmd := exec.Command("node", "--input-type=module", "-e", `
const [a, b] = await Promise.all(process.argv.slice(1).map((f) => import(f)))
const keys = (m) => Object.keys(m).sort().join(' ')
console.log(JSON.stringify(a.result) === JSON.stringify(b.result) && keys(a) === keys(b) ? 'same' : JSON.stringify([a.result, b.result, keys(a), keys(b)]))
`, filepath.Join(dir, "as-written.js"), filepath.Join(dir, "minified.js"))
	got, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(got)) != "same" {
		t.Errorf("node: %v\n%s", err, got)
	}
}

const styles = `@layer reset, tokens, components;

@property --sweep {
  syntax: '<angle>';
  inherits: false;
  initial-value: 0deg;
}

@layer components {
  .card {
    --gap: 1rem;
    --empty: ;
    container: card / inline-size;
    color: color-mix(in oklch, var(--fg) 80%, transparent);
    margin: 0px 0px 0px 0px;

    & > .title { font-weight: 700; }
    &:hover, &:focus-visible { outline: 2px solid currentColor; }
    .dark & { color: white; }

    @container card (width < 30rem) {
      & .meta { display: none; }
    }
    @media (prefers-reduced-motion: reduce) {
      transition: none;
    }
  }

  :is(h1, h2) + p { margin-block-start: 0.5em; }

  @scope ([data-demo="guestbook"]) {
    :scope { display: grid; }
  }
  .quote::after { content: "  two  spaces  "; }
}

@container page (width < 52rem) {
  .layout { grid-template-columns: 1fr; }
}
`

func TestStyles(t *testing.T) {
	out, err := minify.Bytes("text/css", []byte(styles))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) >= len(styles)*9/10 {
		t.Errorf("%d bytes of %d", len(out), len(styles))
	}
	checkCSS(t, "styles", []byte(styles), out)
	for _, kept := range []string{
		"@layer reset,tokens,components;",
		"@layer components{",
		"--empty: ;",
		"container:card/inline-size",
		"&:hover, &:focus-visible{",
		".dark &{",
		"@container card (width < 30rem)",
		"@scope",
		`content:"  two  spaces  "`,
		"@container page (width < 52rem)",
	} {
		if !bytes.Contains(out, []byte(kept)) {
			t.Errorf("no %s in\n%s", kept, out)
		}
	}
}

// tdewolff's CSS minifier minifies a data: URI through its minify.M, which
// Bytes must hand it, and writes each in its shorter encoding.
func TestDataURIs(t *testing.T) {
	in := `.a { background: url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg'/%3E"); }
.b { background: url(data:text/css;base64,LmEgeyBjb2xvcjogcmVkIH0=); }
.c { background: url("data:image/png;base64,iVBORw0KGgo="); }
`
	out, err := minify.Bytes("text/css", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(out, []byte("url(")); n != 3 {
		t.Errorf("%d data URIs in\n%s", n, out)
	}
	for _, want := range []string{"data:text/css,.a%7Bcolor:red%7D", "data:image/png,%89PNG%0D%0A%1A%0A"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("no %s in\n%s", want, out)
		}
	}
}

func TestMediaTypes(t *testing.T) {
	for _, mt := range []string{"text/javascript; charset=utf-8", "application/javascript", "Text/JavaScript"} {
		if out, err := minify.Bytes(mt, []byte("const a = 1 ;")); err != nil || string(out) != "const a=1;" {
			t.Errorf("%s: %q, %v", mt, out, err)
		}
	}
	for _, mt := range []string{"image/svg+xml", "application/json", "text/html", ""} {
		if _, err := minify.Bytes(mt, []byte("{}")); err == nil {
			t.Errorf("%s: no error", mt)
		}
	}
	_, err := minify.Bytes("text/javascript", []byte("const a = {\n  b: 1,,\n}\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("JavaScript that does not parse: %v", err)
	}
}

// TestScripts: a classic script's top-level names are globals, which keep
// their names, and esbuild writes no syntax newer than ES2020 of its own.
func TestScripts(t *testing.T) {
	in := "function helper(longArgument) { return longArgument * 2 }\nvar count = 0\nlet seen\nseen || (seen = helper(count))\n"
	out, err := minify.Bytes("text/javascript", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{"function helper(", "var count=0", "let seen"} {
		if !bytes.Contains(out, []byte(kept)) {
			t.Errorf("no %s in\n%s", kept, out)
		}
	}
	if bytes.Contains(out, []byte("longArgument")) || bytes.Contains(out, []byte("||=")) {
		t.Errorf("a local name kept its length, or ||= (ES2021) came in:\n%s", out)
	}
}

// TestRealFiles minifies the scripts and stylesheets the template and the
// site serve, and two Starbase v0.6.0 components that tdewolff broke: each
// comes out smaller and still parses, and node, where it is installed,
// takes every script it took as written.
func TestRealFiles(t *testing.T) {
	files := map[string][]byte{}
	plugin, err := fs.ReadFile(live.Scripts, live.NewestFrame)
	if err != nil {
		t.Fatal(err)
	}
	files["live/"+live.NewestFrame] = plugin
	for _, dir := range []string{"../template/internal/web/static", "../site/internal/web/static", "testdata"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".js") && !strings.HasSuffix(path, ".css") {
				return err
			}
			b, err := os.ReadFile(path)
			files[strings.TrimPrefix(path, "../")] = b
			return err
		})
		if errors.Is(err, fs.ErrNotExist) {
			t.Logf("%s is not here, as in lith's module zip", dir)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	_, nodeErr := exec.LookPath("node")
	dir := t.TempDir()
	for name, b := range files {
		mt := "text/css"
		if strings.HasSuffix(name, ".js") {
			mt = "text/javascript"
		}
		out, err := minify.Bytes(mt, b)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(out) >= len(b) && !strings.Contains(name, "/vendor/") {
			t.Errorf("%s: %d bytes of %d", name, len(out), len(b))
		}
		if mt == "text/css" {
			checkCSS(t, name, b, out)
			continue
		}
		parseJS(t, out)
		if nodeErr == nil {
			nodeChecks(t, dir, name, b, out)
		}
	}
}

// nodeChecks has node check the minified script as it checks the script as
// written: as a module, or else as a classic script.
func nodeChecks(t *testing.T, dir, name string, written, minified []byte) {
	t.Helper()
	for _, ext := range []string{".mjs", ".cjs"} {
		base := filepath.Join(dir, strings.ReplaceAll(name, "/", "_"))
		os.WriteFile(base+".written"+ext, written, 0o644)
		if exec.Command("node", "--check", base+".written"+ext).Run() != nil {
			continue
		}
		os.WriteFile(base+".min"+ext, minified, 0o644)
		if out, err := exec.Command("node", "--check", base+".min"+ext).CombinedOutput(); err != nil {
			t.Errorf("%s: node --check of the minified %s: %v\n%s", name, ext, err, out)
		}
		return
	}
	t.Errorf("%s: node takes it as written neither as a module nor as a script", name)
}

func parseJS(t *testing.T, b []byte) *js.AST {
	t.Helper()
	ast, err := js.Parse(parse.NewInputBytes(b), js.Options{})
	if err != nil {
		t.Fatalf("%v\n%.300s", err, b)
	}
	return ast
}

// moduleNames lists what a module imports and exports, by the names other
// modules see, in order; local bindings may be renamed.
func moduleNames(ast *js.AST) []string {
	var names []string
	for _, stmt := range ast.List {
		switch s := stmt.(type) {
		case *js.ImportStmt:
			from := strings.Trim(string(s.Module), `'"`)
			if len(s.Default) > 0 {
				names = append(names, "import default from "+from)
			}
			for _, a := range s.List {
				imported := a.Name
				if len(imported) == 0 {
					imported = a.Binding
				}
				names = append(names, "import "+string(imported)+" from "+from)
			}
			if len(s.Default) == 0 && len(s.List) == 0 {
				names = append(names, "import "+from)
			}
		case *js.ExportStmt:
			e := "export"
			if s.Default {
				e += " default"
			}
			if s.Module != nil {
				e += " from " + strings.Trim(string(s.Module), `'"`)
			}
			switch d := s.Decl.(type) {
			case *js.FuncDecl:
				e += " function"
				if !s.Default {
					e += " " + name(d.Name)
				}
			case *js.ClassDecl:
				e += " class"
				if !s.Default {
					e += " " + name(d.Name)
				}
			case *js.VarDecl:
				for _, b := range d.List {
					e += " " + d.TokenType.String() + " " + name(b.Binding)
				}
			}
			for _, a := range s.List {
				e += " " + string(a.Binding)
			}
			names = append(names, e)
		}
	}
	return names
}

func name(n js.INode) string {
	if v, ok := n.(*js.Var); ok && v != nil {
		return string(v.Data)
	}
	return fmt.Sprint(n)
}

// taggedText returns the text of every tagged template, as its tag sees it.
func taggedText(ast *js.AST) []string {
	var v tagged
	js.Walk(&v, ast)
	return v.texts
}

type tagged struct{ texts []string }

func (v *tagged) Enter(n js.INode) js.IVisitor {
	if t, ok := n.(*js.TemplateExpr); ok && t.Tag != nil {
		var s strings.Builder
		for _, p := range t.List {
			s.Write(p.Value)
			s.WriteString("${}")
		}
		s.Write(t.Tail)
		v.texts = append(v.texts, s.String())
	}
	return v
}

func (v *tagged) Exit(js.INode) {}

// checkCSS fails when the minified CSS has parse errors that the CSS as
// written did not have.
func checkCSS(t *testing.T, name string, written, minified []byte) {
	t.Helper()
	if a, b := cssErrors(written), cssErrors(minified); b > a {
		t.Errorf("%s: %d parse errors as written, %d minified:\n%.300s", name, a, b, minified)
	}
}

func cssErrors(b []byte) int {
	p := css.NewParser(parse.NewInputBytes(b), false)
	n := 0
	for {
		gt, _, _ := p.Next()
		if gt == css.ErrorGrammar {
			if !p.HasParseError() {
				if p.Err() != io.EOF {
					n++
				}
				return n
			}
			n++
		}
	}
}

func write(t *testing.T, dir, name, text string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}
