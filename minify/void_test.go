package minify_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zweiundeins/lith/minify"
)

// voids are the shapes that tdewolff's JavaScript minifier (v2.24.19 to at
// least v2.24.24) emptied: the side effect after void of a logical
// expression went, as in Starbase v0.6.0's sb-busy and sb-virtual-scroll.
// Each sets result, which node reads as written and minified.
var voids = []struct{ name, body, want string }{
	{"function", `let calls = 0
const b = () => { calls++ }
function f(a) { return void (a || b()) }
result = [f(false), calls]`, `[null,1]`},
	{"early return", `let calls = 0, x = true
const b = () => { calls++ }
function f(a) { if (x) return void (a || b()); return 1 }
result = [f(false), calls]`, `[null,1]`},
	{"arrow", `let calls = 0
const b = () => { calls++ }
const g = (a) => void (a || b())
result = [g(false), calls]`, `[null,1]`},
	{"assignment", `let b = 0
function f(a) { return void (a || (b = 1)) }
result = [f(false), b]`, `[null,1]`},
	{"logical assignment", `let bailed = false
function f(type) { return void (bailed ||= type === 'error') }
result = [f('error'), bailed]`, `[null,true]`},
	{"call", `let calls = 0
const b = () => { calls++ }
function f() { return void b() }
result = [f(), calls]`, `[null,1]`},
}

// TestVoidKeepsSideEffects runs every shape in node, as a classic script
// and as a module, minified and as written.
func TestVoidKeepsSideEffects(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	for _, v := range voids {
		for _, form := range []struct{ ext, head, tail string }{
			{".cjs", "var result\n", "\nglobalThis.result = result\n"},
			{".mjs", "let result\n", "\nglobalThis.result = result\nexport { result }\n"},
		} {
			src := form.head + v.body + form.tail
			out, err := minify.Bytes("text/javascript", []byte(src))
			if err != nil {
				t.Fatalf("%s%s: %v", v.name, form.ext, err)
			}
			base := filepath.Join(dir, strings.ReplaceAll(v.name, " ", "-"))
			write(t, dir, filepath.Base(base)+".written"+form.ext, src)
			write(t, dir, filepath.Base(base)+".min"+form.ext, string(out))
			for _, file := range []string{base + ".written" + form.ext, base + ".min" + form.ext} {
				got, err := exec.Command("node", "--input-type=module", "-e", `
const f = process.argv[1]
if (f.endsWith('.cjs')) (await import('node:module')).createRequire(import.meta.url)(f)
else await import(f)
console.log(JSON.stringify(globalThis.result))
`, file).CombinedOutput()
				if err != nil || strings.TrimSpace(string(got)) != v.want {
					t.Errorf("%s%s: %s gives %s, want %s (%v)\nminified: %s", v.name, form.ext, filepath.Base(file), got, v.want, err, out)
				}
			}
		}
	}
}
