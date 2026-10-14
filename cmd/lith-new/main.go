// Command lith-new starts a project from lith's template (template/ in this
// repository): a small guestbook on SQLite and Datastar, with its tests,
// deploy files and CI.
//
//	go run github.com/zweiundeins/lith/cmd/lith-new [-lith dir] [-name name] <dir> <module path>
//
// See lith-new -h.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/zweiundeins/lith/template"
)

const (
	lithModule     = "github.com/zweiundeins/lith"
	templateModule = lithModule + "/template"
	templateName   = "app"
)

const usage = `usage: lith-new [-lith dir] [-name name] <dir> <module path>

lith-new copies lith's project template into dir, which must be new or empty,
as the module <module path>: a guestbook on SQLite and Datastar with its tests,
deploy files and CI. The app is named after the module path's last element, or
-name: its binary in cmd/<name>, the systemd units in deploy/, the <NAME>_
environment variables.

The new go.mod requires the lith that lith-new was run from: with
go run github.com/zweiundeins/lith/cmd/lith-new@latest, the latest release.
To work against a lith checkout instead, name it with -lith, or with LITH_DIR
in the environment, and the new go.mod gets

	replace github.com/zweiundeins/lith => <the checkout's absolute path>

Flags:
`

func main() {
	log.SetFlags(0)
	log.SetPrefix("lith-new: ")
	f := flag.NewFlagSet("lith-new", flag.ExitOnError)
	lith := f.String("lith", os.Getenv("LITH_DIR"), "the `dir` of a lith checkout, for the replace directive (default $LITH_DIR)")
	name := f.String("name", "", "the app's `name` (default: the module path's last element)")
	f.Usage = func() {
		fmt.Fprint(f.Output(), usage)
		f.PrintDefaults()
	}
	f.Parse(os.Args[1:])
	if f.NArg() != 2 {
		f.Usage()
		os.Exit(2)
	}
	p, err := newProject(f.Arg(0), f.Arg(1), *name, *lith)
	if err != nil {
		log.Fatal(err)
	}
	p.version = version()
	if err := p.write(template.FS); err != nil {
		log.Fatal(err)
	}
	p.next(os.Stdout)
}

// project is what lith-new writes.
type project struct {
	dir, module, name string
	lith              string // absolute path of a lith checkout, or ""
	version           string // the lith version go.mod requires without lith, or ""
}

// zeroVersion is the template's placeholder for lith's version.
const zeroVersion = "v0.0.0-00010101000000-000000000000"

// version is the lith version this binary was built from, as go run
// github.com/zweiundeins/lith/cmd/lith-new@<version> resolved it, or "" for
// a build in a checkout.
func version() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi.Main.Path != lithModule || bi.Main.Version == "(devel)" {
		return ""
	}
	return bi.Main.Version
}

func newProject(dir, module, name, lith string) (*project, error) {
	if err := checkModule(module); err != nil {
		return nil, err
	}
	if name == "" {
		name = nameOf(module)
	}
	if !validName.MatchString(name) {
		return nil, fmt.Errorf("the app's name %q is not a lowercase word of up to 32 letters, digits, - and _: choose one with -name", name)
	}
	if lith != "" {
		abs, err := filepath.Abs(lith)
		if err != nil {
			return nil, err
		}
		if err := checkLith(abs); err != nil {
			return nil, err
		}
		lith = abs
	}
	return &project{dir: dir, module: module, name: name, lith: lith}, nil
}

var (
	moduleElem = regexp.MustCompile(`^[A-Za-z0-9_~-]([A-Za-z0-9._~-]*[A-Za-z0-9_~-])?$`)
	majorElem  = regexp.MustCompile(`^v[0-9]+$`)
	validName  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	plainPath  = regexp.MustCompile(`^[A-Za-z0-9/._~+-]+$`)
)

func checkModule(module string) error {
	for e := range strings.SplitSeq(module, "/") {
		if !moduleElem.MatchString(e) {
			return fmt.Errorf("%q is not a module path, such as example.com/guestbook", module)
		}
	}
	if module == lithModule || strings.HasPrefix(module, lithModule+"/") {
		return fmt.Errorf("%q is lith's: give the project a module path of its own", module)
	}
	return nil
}

// nameOf names the app as go build names a binary: after the module path's
// last element, or the one before a major version suffix.
func nameOf(module string) string {
	elems := strings.Split(module, "/")
	name := elems[len(elems)-1]
	if len(elems) > 1 && majorElem.MatchString(name) {
		name = elems[len(elems)-2]
	}
	return strings.ToLower(name)
}

func checkLith(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return fmt.Errorf("-lith: %w", err)
	}
	for line := range strings.Lines(string(b)) {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "module" && strings.Trim(f[1], `"`) == lithModule {
			return nil
		}
	}
	return fmt.Errorf("-lith: %s is not a checkout of %s", dir, lithModule)
}

// write copies the template into p.dir.
func (p *project) write(tmpl fs.FS) error {
	if err := newDir(p.dir); err != nil {
		return err
	}
	rewrite := p.rewriter()
	return fs.WalkDir(tmpl, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path == "template.go" {
			return err
		}
		b, err := fs.ReadFile(tmpl, path)
		if err != nil {
			return err
		}
		if !vendored(path) {
			b = []byte(rewrite(string(b)))
		}
		path = rewrite(path)
		if path == "go.mod.tmpl" {
			path = "go.mod"
			switch {
			case p.lith != "":
				b = fmt.Appendf(b, "\nreplace %s => %s\n", lithModule, modPath(p.lith))
			case p.version != "":
				b = bytes.Replace(b, []byte(lithModule+" "+zeroVersion), []byte(lithModule+" "+p.version), 1)
			}
		}
		mode := fs.FileMode(0o644)
		if bytes.HasPrefix(b, []byte("#!")) {
			mode = 0o755
		}
		dst := filepath.Join(p.dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, b, mode)
	})
}

// rewriter returns a function that replaces, in one pass, the template's
// module path and its app's name, app, with the project's: in file names,
// in text, as the prefix APP_ of environment variables, and as the prefix
// app_ of identifiers, such as Alloy's component labels, with - as _.
func (p *project) rewriter() func(string) string {
	re := regexp.MustCompile(regexp.QuoteMeta(templateModule) + `|\b` + templateName + `\b|\b` + templateName + `_|\b` + strings.ToUpper(templateName) + `_`)
	ident := strings.ReplaceAll(p.name, "-", "_") + "_"
	return func(s string) string {
		return re.ReplaceAllStringFunc(s, func(m string) string {
			switch m {
			case templateModule:
				return p.module
			case templateName:
				return p.name
			case templateName + "_":
				return ident
			case strings.ToUpper(templateName) + "_":
				return strings.ToUpper(ident)
			}
			return m
		})
	}
}

// vendored files are copied as they are.
func vendored(path string) bool {
	return strings.HasPrefix(path, "vendor/") || strings.Contains(path, "/vendor/")
}

func newDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return os.MkdirAll(dir, 0o755)
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s is not empty", dir)
	}
	return nil
}

// modPath quotes a directory for go.mod when it holds more than letters,
// digits and the punctuation of plain paths.
func modPath(dir string) string {
	if plainPath.MatchString(dir) {
		return dir
	}
	return strconv.Quote(dir)
}

func (p *project) next(w io.Writer) {
	fmt.Fprintf(w, "Created %s: module %s, app %s.\n", p.dir, p.module, p.name)
	replace := ""
	switch {
	case p.lith != "":
		fmt.Fprintf(w, "lith comes from %s, through a replace in go.mod.\n", p.lith)
		fmt.Fprintln(w, "CI cannot reach that path: drop the replace before you push.")
	case p.version != "":
		fmt.Fprintf(w, "go.mod requires lith %s.\n", p.version)
	default:
		replace = fmt.Sprintf("\tgo get %s@latest\n", lithModule)
	}
	fmt.Fprintf(w, `
Next:

	cd %s
%s	go mod tidy
	go tool task test
	go tool task dev    # serves http://127.0.0.1:8080
	git init && git add -A && git commit -m "Start from the lith template"

AGENTS.md says where things are, deploy/README.md how to run the app on a server.
`, p.dir, replace)
}
