package gen_test

import (
	"strings"
	"testing"

	"github.com/zweiundeins/lith/site/internal/docs/gen"
)

// TestUpToDate fails when the generated parts of the docs differ from what
// cmd/docgen would write now: the reference after a doc comment changed,
// a chart after its CSV file was measured again. Run
//
//	go generate ./content
//
// in site/ and commit the result.
func TestUpToDate(t *testing.T) {
	root, err := gen.Root(".")
	if err != nil {
		t.Fatal(err)
	}
	files, err := gen.Generate(root)
	if err != nil {
		t.Fatal(err)
	}
	diffs, err := gen.Diff(root, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) > 0 {
		t.Errorf("the docs' generated files are out of date; run go generate ./content in site/:\n%s", strings.Join(diffs, "\n"))
	}
	for _, dir := range gen.Packages {
		if _, ok := files[gen.Docs+"/reference/"+gen.Slug(dir)+".md"]; !ok {
			t.Errorf("no reference page for %s", dir)
		}
	}
}
