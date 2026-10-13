// Command vendor-starbase copies Starbase's components into the site. From
// a pinned catalog snapshot of Starbase (pins.json: the site's URL, the
// snapshot's hash, the components wanted) it fetches each component's
// one-file bundle, the bundles of the components it renders, and the files
// it loads on first use (ECharts' library), checks every file against the
// integrity in the snapshot's importmap.json, and writes them under
// internal/web/static/vendor/starbase/<slug>/ with lock.json, the list of
// files and their integrity that the site loads them by and its tests
// check. It refuses a snapshot whose Datastar build is not the one the site
// vendors (internal/web/static/vendor/datastar-rocket.js).
//
//	go run ./cmd/vendor-starbase    (in site/; go tool task vendor-starbase)
//
// Run it again after a change to pins.json: it rewrites the components'
// folders and lock.json. The licences beside them (Starbase's MIT, Apache
// ECharts' LICENSE and NOTICE) come from Starbase's repository, which the
// snapshot does not serve: copy them again when a new component brings
// vendored code with a licence of its own.
package main

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	dir      = "internal/web/static/vendor/starbase"
	datastar = "internal/web/static/vendor/datastar-rocket.js"
)

// Pins is what to vendor, from which snapshot.
type Pins struct {
	Base       string   `json:"base"`
	Catalog    string   `json:"catalog"`
	Components []string `json:"components"`
}

// Lock is what was vendored: lock.json.
type Lock struct {
	Base     string `json:"base"`
	Catalog  string `json:"catalog"`
	Datastar struct {
		URL       string `json:"url"`
		Integrity string `json:"integrity"`
	} `json:"datastar"`
	Components []Component `json:"components"`
}

// Component is one vendored component.
type Component struct {
	Slug string `json:"slug"`
	Tag  string `json:"tag"`
	Hash string `json:"hash"`
	// Pinned: listed in pins.json; else vendored because a pinned one
	// renders it.
	Pinned  bool     `json:"pinned"`
	Bundle  string   `json:"bundle"`
	Renders []string `json:"renders,omitempty"`
	Lazy    []string `json:"lazy,omitempty"`
	// Files are the component's files under its folder, with their
	// integrity: the bundle and the lazy files.
	Files map[string]string `json:"files"`
}

var (
	tagRe     = regexp.MustCompile(`<(sb-[a-z0-9]+(?:-[a-z0-9]+)*)`)
	dynamicRe = regexp.MustCompile(`import\(\s*["'](\./[^"']+)["']\s*\)`)
	staticRe  = regexp.MustCompile(`(?:from|import)\s*["'](\./[^"']+)["']`)
	versionRe = regexp.MustCompile(`^([a-z0-9-]+)@([0-9a-f]{12})/(.+)$`)
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("vendor-starbase: ")
	flag.Parse()
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var pins Pins
	if err := readJSON(filepath.Join(dir, "pins.json"), &pins); err != nil {
		return err
	}
	base := strings.TrimRight(pins.Base, "/") + "/c/"
	var snapshot struct {
		Integrity map[string]string `json:"integrity"`
	}
	body, err := fetch(base+"@"+pins.Catalog+"/importmap.json", "")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, &snapshot); err != nil {
		return fmt.Errorf("importmap.json: %w", err)
	}
	// The snapshot's files by component: slug -> hash, and path -> integrity.
	versions := map[string]string{}
	files := map[string]map[string]string{}
	for url, sri := range snapshot.Integrity {
		m := versionRe.FindStringSubmatch(strings.TrimPrefix(url, base))
		if m == nil {
			continue
		}
		versions[m[1]] = m[2]
		if files[m[1]] == nil {
			files[m[1]] = map[string]string{}
		}
		files[m[1]][m[3]] = sri
	}

	lock := Lock{Base: pins.Base, Catalog: pins.Catalog}
	own, err := os.ReadFile(datastar)
	if err != nil {
		return err
	}
	dsHash := versions["datastar"]
	lock.Datastar.URL = base + "datastar@" + dsHash + "/datastar-rocket.js"
	lock.Datastar.Integrity = files["datastar"]["datastar-rocket.js"]
	if dsHash == "" || lock.Datastar.Integrity != integrity(own) {
		return fmt.Errorf("the snapshot pins Datastar %s (%s), the site vendors %s: update %s first", lock.Datastar.URL, lock.Datastar.Integrity, integrity(own), datastar)
	}

	out := map[string][]byte{}
	queue := slices.Clone(pins.Components)
	done := map[string]*Component{}
	for len(queue) > 0 {
		slug := queue[0]
		queue = queue[1:]
		if done[slug] != nil {
			continue
		}
		hash := versions[slug]
		if hash == "" {
			return fmt.Errorf("%s is not in snapshot %s", slug, pins.Catalog)
		}
		c := &Component{Slug: slug, Tag: "sb-" + slug, Hash: hash, Pinned: slices.Contains(pins.Components, slug),
			Bundle: slug + ".bundle.min.js", Files: map[string]string{}}
		done[slug] = c
		get := func(p string) ([]byte, error) {
			sri := files[slug][p]
			if sri == "" {
				return nil, fmt.Errorf("%s@%s/%s is not in the snapshot's importmap.json", slug, hash, p)
			}
			b, err := fetch(base+slug+"@"+hash+"/"+p, sri)
			if err != nil {
				return nil, err
			}
			c.Files[p] = sri
			out[slug+"/"+p] = b
			return b, nil
		}
		bundle, err := get(c.Bundle)
		if err != nil {
			return err
		}
		for _, m := range tagRe.FindAllStringSubmatch(string(bundle), -1) {
			dep := strings.TrimPrefix(m[1], "sb-")
			if dep != slug && versions[dep] != "" && !slices.Contains(c.Renders, dep) {
				c.Renders = append(c.Renders, dep)
				queue = append(queue, dep)
			}
		}
		// What the bundle loads on first use, and what that imports.
		pending := matches(dynamicRe, bundle, path.Dir(c.Bundle))
		for len(pending) > 0 {
			p := pending[0]
			pending = pending[1:]
			if c.Files[p] != "" {
				continue
			}
			b, err := get(p)
			if err != nil {
				return err
			}
			c.Lazy = append(c.Lazy, p)
			pending = append(pending, matches(dynamicRe, b, path.Dir(p))...)
			pending = append(pending, matches(staticRe, b, path.Dir(p))...)
		}
		slices.Sort(c.Lazy)
	}
	for _, c := range done {
		lock.Components = append(lock.Components, *c)
	}
	slices.SortFunc(lock.Components, func(a, b Component) int { return strings.Compare(a.Slug, b.Slug) })

	// Rewrite the folder: the files, and the lock beside pins.json.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	for name, b := range out {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "lock.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	var n int
	for _, c := range lock.Components {
		n += len(c.Files)
	}
	log.Printf("%d components, %d files, from snapshot %s", len(lock.Components), n, pins.Catalog)
	return nil
}

// matches returns the relative imports a pattern finds in b, resolved
// against dir and cleaned, inside the component's folder.
func matches(re *regexp.Regexp, b []byte, dir string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		p := path.Clean(path.Join(dir, m[1]))
		if !strings.HasPrefix(p, "../") && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

var client = &http.Client{Timeout: 60 * time.Second}

// fetch gets url and, when sri is given, checks the body against it.
func fetch(url, sri string) ([]byte, error) {
	res, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, res.Status)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if sri != "" && integrity(b) != sri {
		return nil, fmt.Errorf("%s: integrity %s, the snapshot says %s", url, integrity(b), sri)
	}
	return b, nil
}

// integrity is b's SRI: sha384, base64.
func integrity(b []byte) string {
	sum := sha512.Sum384(b)
	return "sha384-" + base64.StdEncoding.EncodeToString(sum[:])
}

func readJSON(name string, v any) error {
	b, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errors.Join(fmt.Errorf("%s", name), err)
	}
	return nil
}
