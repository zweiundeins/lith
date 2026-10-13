package docs_test

import (
	"encoding/csv"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zweiundeins/lith/site/content"
	"github.com/zweiundeins/lith/site/internal/docs/gen"
	"github.com/zweiundeins/lith/site/internal/markdown"
)

// fence finds the fenced blocks of a page: the info string and the body.
var fence = regexp.MustCompile("(?ms)^```([^\n`]*)\n(.*?)^```")

// pages yields every Markdown file of the docs with its text.
func pages(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := fs.WalkDir(content.Docs, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		b, err := fs.ReadFile(content.Docs, path)
		files[path] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func root(t *testing.T) string {
	t.Helper()
	r, err := gen.Root(".")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var space = regexp.MustCompile(`\s+`)

func squash(s string) string { return strings.TrimSpace(space.ReplaceAllString(s, " ")) }

// TestListings checks every listing with a source: its code must be in
// that file of lith's repository, whitespace aside, so a page cannot show
// code that the repository does not hold. The examples the guides show
// are packages under internal/docs/example, compiled and tested with the
// site.
func TestListings(t *testing.T) {
	r := root(t)
	n := 0
	for path, text := range pages(t) {
		for _, m := range fence.FindAllStringSubmatch(text, -1) {
			_, _, source := markdown.Info(m[1])
			if source == "" {
				continue
			}
			n++
			b, err := os.ReadFile(filepath.Join(r, filepath.FromSlash(source)))
			if err != nil {
				t.Errorf("%s: a listing of %s: %v", path, source, err)
				continue
			}
			if !strings.Contains(squash(string(b)), squash(m[2])) {
				t.Errorf("%s: the listing of %s is not in the file:\n%s", path, source, m[2])
			}
		}
	}
	if n == 0 {
		t.Error("no listing has a source")
	}
}

var digits = regexp.MustCompile(`-?\d[\d,]*(\.\d+)?`)

// TestChartNumbers checks that every number a chart draws appears in the
// file its source names: a field of a CSV file, or a number in a page of
// results, its thousands separators aside.
func TestChartNumbers(t *testing.T) {
	r := root(t)
	known := map[string]map[string]bool{}
	numbers := func(source string) map[string]bool {
		if known[source] != nil {
			return known[source]
		}
		set := map[string]bool{}
		b, err := os.ReadFile(filepath.Join(r, filepath.FromSlash(source)))
		if err != nil {
			t.Errorf("chart source %s: %v", source, err)
		}
		if strings.HasSuffix(source, ".csv") {
			records, err := csv.NewReader(strings.NewReader(string(b))).ReadAll()
			if err != nil {
				t.Errorf("chart source %s: %v", source, err)
			}
			for _, rec := range records {
				for _, f := range rec {
					set[f] = true
				}
			}
		} else {
			for _, d := range digits.FindAllString(string(b), -1) {
				set[strings.ReplaceAll(d, ",", "")] = true
			}
		}
		known[source] = set
		return set
	}
	charts := 0
	for path, text := range pages(t) {
		for _, m := range fence.FindAllStringSubmatch(text, -1) {
			if lang, _, _ := markdown.Info(m[1]); lang != "chart" {
				continue
			}
			charts++
			c, err := markdown.ParseChart(m[2])
			if err != nil {
				t.Errorf("%s: %v", path, err)
				continue
			}
			set := numbers(c.Source)
			for _, row := range c.Rows {
				if !set[row.Y] {
					t.Errorf("%s: chart %q draws %s, which %s does not hold", path, c.Title, row.Y, c.Source)
				}
			}
		}
	}
	if charts == 0 {
		t.Error("no charts")
	}
}

// TestBackupExample vets the Litestream example, a module of its own so
// that the site never pulls in Litestream: the site's go vet does not
// reach it. It needs that module's dependencies, from the module cache or
// the network.
func TestBackupExample(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command")
	}
	cmd := exec.Command(gobin, "vet", "./...")
	cmd.Dir = filepath.Join("example", "backup")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go vet in %s: %v\n%s", cmd.Dir, err, out)
	}
}
