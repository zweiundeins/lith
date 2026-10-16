package main

import (
	"bytes"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zweiundeins/lith/template"
)

func TestWrite(t *testing.T) {
	lith, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "visitors")
	p, err := newProject(dir, "example.com/acme/visitor-log", "", lith)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.write(template.FS); err != nil {
		t.Fatal(err)
	}

	gomod := read(t, dir, "go.mod")
	replace := "\nreplace (\n\tgithub.com/zweiundeins/lith => " + lith + "\n\tgithub.com/zweiundeins/lith/minify => " + filepath.Join(lith, "minify") + "\n)\n"
	if !strings.HasPrefix(gomod, "module example.com/acme/visitor-log\n") || !strings.HasSuffix(gomod, replace) {
		t.Errorf("go.mod:\n%s", gomod)
	}
	main := read(t, dir, "cmd/visitor-log/main.go")
	for _, want := range []string{`fmt.Println("visitor-log", `, `"VISITOR_LOG_" + strings.ToUpper(fl.Name)`, `"example.com/acme/visitor-log/internal/web"`} {
		if !strings.Contains(main, want) {
			t.Errorf("main.go lacks %s", want)
		}
	}
	if s := read(t, dir, "deploy/visitor-log.service"); !strings.Contains(s, "ExecStart=/opt/visitor-log/visitor-log serve") {
		t.Errorf("the unit:\n%s", s)
	}
	if fi, err := os.Stat(filepath.Join(dir, "deploy/visitor-log-deploy")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the deploy script: %v %v", fi, err)
	}
	const accessLog = "/var/log/caddy/visitor-log-access.log"
	if s := read(t, dir, "deploy/visitor-log.caddy"); !strings.Contains(s, accessLog) {
		t.Errorf("the Caddy block:\n%s", s)
	}
	if s := read(t, dir, "deploy/observability/visitor-log.targets.yml"); !strings.Contains(s, "service: visitor-log,") {
		t.Errorf("the probes:\n%s", s)
	}
	alloy := read(t, dir, "deploy/observability/visitor-log.alloy")
	for _, want := range []string{`"SYSLOG_IDENTIFIER=visitor-log"`, `replacement  = "visitor-log"`, `"site" = "visitor-log"`, accessLog} {
		if !strings.Contains(alloy, want) {
			t.Errorf("the Alloy file lacks %s", want)
		}
	}
	// Alloy combines every file of its folder: labels are identifiers, and the app's own.
	labels := alloyLabel.FindAllStringSubmatch(alloy, -1)
	for _, m := range labels {
		if !strings.HasPrefix(m[1], "visitor_log_") || !identifier.MatchString(m[1]) {
			t.Errorf("the Alloy component label %q", m[1])
		}
	}
	if len(labels) < 5 {
		t.Errorf("the Alloy file has %d components", len(labels))
	}
	for _, gone := range []string{"template.go", "go.mod.tmpl", "cmd/app", "deploy/app.service", "deploy/observability/app.alloy"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s was copied", gone)
		}
	}
	vendored, _ := fs.ReadFile(template.FS, "internal/web/static/vendor/datastar-rocket.js")
	if read(t, dir, "internal/web/static/vendor/datastar-rocket.js") != string(vendored) {
		t.Error("the vendored Datastar build changed on the way")
	}

	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte(templateModule)) {
			t.Errorf("%s still names %s", path, templateModule)
		}
		if strings.HasSuffix(path, ".go") {
			if _, err := parser.ParseFile(token.NewFileSet(), path, b, parser.SkipObjectResolution); err != nil {
				t.Error(err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRewriter(t *testing.T) {
	p := &project{module: "example.com/acme/visitor-log", name: "visitor-log"}
	got := p.rewriter()("app app_journal APP_DB application mapp_x github.com/zweiundeins/lith/template/internal")
	want := "visitor-log visitor_log_journal VISITOR_LOG_DB application mapp_x example.com/acme/visitor-log/internal"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestPublishable keeps every template file in lith's module zip, which
// leaves out the files in subdirectories of vendor directories.
func TestPublishable(t *testing.T) {
	fs.WalkDir(template.FS, ".", func(path string, d fs.DirEntry, err error) error {
		elems := strings.Split(path, "/")
		for i, e := range elems {
			if e == "vendor" && i < len(elems)-2 {
				t.Errorf("%s would be missing from lith's module zip", path)
			}
		}
		return err
	})
}

func TestNames(t *testing.T) {
	for module, want := range map[string]string{
		"example.com/acme/guestbook":    "guestbook",
		"github.com/acme/Shop/v2":       "shop",
		"kanban":                        "kanban",
		"example.com/acme/my.site":      "",
		"github.com/zweiundeins/lith/x": "",
		"./guestbook":                   "",
	} {
		p, err := newProject(t.TempDir(), module, "", "")
		switch {
		case want == "" && err == nil:
			t.Errorf("%s: no error, name %q", module, p.name)
		case want != "" && (err != nil || p.name != want):
			t.Errorf("%s: %v, want the name %q", module, err, want)
		}
	}
	if _, err := newProject(t.TempDir(), "example.com/x", "", t.TempDir()); err == nil {
		t.Error("-lith accepted a directory without lith's go.mod")
	}
}

func TestNotEmpty(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "keep"), nil, 0o644)
	p, err := newProject(dir, "example.com/x", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.write(template.FS); err == nil {
		t.Error("wrote into a directory that is not empty")
	}
}

// TestGenerated builds and tests a generated project against this checkout.
// It needs the project's modules, from the network or the module cache.
func TestGenerated(t *testing.T) {
	if os.Getenv("LITH_NEW_E2E") == "" {
		t.Skip("set LITH_NEW_E2E=1 to tidy, vet, test and build a generated project")
	}
	lith, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "visitors")
	p, err := newProject(dir, "example.com/acme/visitor-log", "", lith)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.write(template.FS); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"mod", "tidy"},
		{"vet", "./..."},
		{"test", "-race", "./..."},
		{"build", "-o", os.DevNull, "./cmd/visitor-log"},
	} {
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

var (
	alloyLabel = regexp.MustCompile(`(?m)^[a-z_.]+ "([^"]*)" \{`)
	identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Without a checkout, go.mod requires the lith version lith-new ran from,
// and lith/minify of that version, so that go mod tidy finds them; with
// one, the placeholders stay and replace directives point at the checkout.
func TestWriteVersion(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "visitors")
	p, err := newProject(dir, "example.com/acme/visitor-log", "", "")
	if err != nil {
		t.Fatal(err)
	}
	p.version = "v0.4.2"
	if err := p.write(template.FS); err != nil {
		t.Fatal(err)
	}
	gomod := read(t, dir, "go.mod")
	if !strings.Contains(gomod, "\tgithub.com/zweiundeins/lith v0.4.2\n") || !strings.Contains(gomod, "\tgithub.com/zweiundeins/lith/minify v0.4.2\n") ||
		strings.Contains(gomod, zeroVersion) || strings.Contains(gomod, "replace") {
		t.Errorf("go.mod:\n%s", gomod)
	}
	var out bytes.Buffer
	p.next(&out)
	if !strings.Contains(out.String(), "requires lith v0.4.2") || strings.Contains(out.String(), "go get") {
		t.Errorf("next steps:\n%s", out.String())
	}

	p.version = ""
	out.Reset()
	p.next(&out)
	if !strings.Contains(out.String(), "go get github.com/zweiundeins/lith@latest github.com/zweiundeins/lith/minify@latest\n") {
		t.Errorf("next steps without a version:\n%s", out.String())
	}
}
