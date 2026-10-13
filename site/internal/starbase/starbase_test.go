package starbase_test

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zweiundeins/lith/site/internal/starbase"
)

const static = "../web/static"

// TestVendored checks the vendored components against lock.json, without
// the network: every file in the lock is there with its integrity, nothing
// else is, every pinned component is in the lock, and the Datastar build
// the snapshot pins is the one the site vendors.
func TestVendored(t *testing.T) {
	fsys := os.DirFS(static)
	set, err := starbase.Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	var pins struct {
		Components []string `json:"components"`
	}
	b, err := fs.ReadFile(fsys, starbase.Dir+"/pins.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &pins); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, c := range set.Lock.Components {
		if c.Files[c.Bundle] == "" {
			t.Errorf("%s: its bundle %s is not among its files", c.Slug, c.Bundle)
		}
		for _, d := range c.Renders {
			if !set.Has(d) {
				t.Errorf("%s renders %s, which is not vendored", c.Slug, d)
			}
		}
		for p, sri := range c.Files {
			want[starbase.Dir+"/"+c.Slug+"/"+p] = sri
		}
	}
	for _, slug := range pins.Components {
		if !set.Has(slug) {
			t.Errorf("%s is pinned but not in lock.json: run go run ./cmd/vendor-starbase", slug)
		}
	}
	err = fs.WalkDir(fsys, starbase.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Dir(p) == starbase.Dir && (path.Ext(p) == ".json" || path.Ext(p) == ".txt") {
			return err // pins.json, lock.json and the licences
		}
		sri, ok := want[p]
		if !ok {
			t.Errorf("%s is not in lock.json", p)
			return nil
		}
		delete(want, p)
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		if got := integrity(body); got != sri {
			t.Errorf("%s: integrity %s, lock.json says %s", p, got, sri)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for p := range want {
		t.Errorf("%s is in lock.json but missing", p)
	}
	ds, err := os.ReadFile(filepath.Join(static, "vendor/datastar-rocket.js"))
	if err != nil {
		t.Fatal(err)
	}
	if integrity(ds) != set.Lock.Datastar.Integrity {
		t.Errorf("the site vendors Datastar %s, the snapshot pins %s", integrity(ds), set.Lock.Datastar.Integrity)
	}
}

func TestScripts(t *testing.T) {
	set, err := starbase.Load(os.DirFS(static))
	if err != nil {
		t.Fatal(err)
	}
	files, err := set.Scripts("data-table", "drawer", "virtual-scroll")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, strings.TrimPrefix(f.Name, starbase.Dir+"/"))
		if !strings.HasPrefix(f.Integrity, "sha384-") {
			t.Errorf("%s: integrity %q", f.Name, f.Integrity)
		}
	}
	// What a component renders loads before it, and each bundle once.
	if !slices.Equal(names, []string{"virtual-scroll/virtual-scroll.bundle.min.js", "data-table/data-table.bundle.min.js", "drawer/drawer.bundle.min.js"}) {
		t.Errorf("scripts: %v", names)
	}
	if _, err := set.Scripts("no-such-component"); err == nil {
		t.Error("an unknown component was accepted")
	}
}

func integrity(b []byte) string {
	sum := sha512.Sum384(b)
	return "sha384-" + base64.StdEncoding.EncodeToString(sum[:])
}
