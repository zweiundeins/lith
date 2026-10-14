package gen

import (
	"os"
	"path/filepath"
	"testing"
)

// A part between an include line and its end comes from the file the line
// names, and a page without one stays as it is.
func TestInclude(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "bench"), 0o755)
	os.WriteFile(filepath.Join(root, "bench", "t.md"), []byte("| a |\n| --- |\n| 1 |\n"), 0o644)
	page := "Before.\n\n<!-- include: bench/t.md -->\n| old |\n<!-- end include -->\n\nAfter.\n"
	got, found, err := include(root, page)
	want := "Before.\n\n<!-- include: bench/t.md -->\n| a |\n| --- |\n| 1 |\n<!-- end include -->\n\nAfter.\n"
	if err != nil || !found || got != want {
		t.Fatalf("include: %v, %v:\n%s", found, err, got)
	}
	if got, found, err := include(root, "No part.\n"); err != nil || found || got != "No part.\n" {
		t.Fatalf("a page without a part: %v, %v, %q", found, err, got)
	}
	if _, _, err := include(root, "<!-- include: bench/none.md -->\n<!-- end include -->"); err == nil {
		t.Fatal("a missing file included nothing and no error")
	}
}
