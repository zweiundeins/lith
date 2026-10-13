// Package starbase serves the Starbase components the site vendors: Rocket
// components from github.com/zweiundeins/starbase, copied as one-file
// bundles into static/vendor/starbase by cmd/vendor-starbase, from a pinned
// catalog snapshot, each with its integrity in lock.json. A page names the
// components it uses; Scripts returns the bundles to load for them, the
// components they render included.
package starbase

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"
)

// Dir is the vendored folder inside the static files.
const Dir = "vendor/starbase"

// Lock is lock.json, as cmd/vendor-starbase writes it.
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
	Slug    string            `json:"slug"`
	Tag     string            `json:"tag"`
	Hash    string            `json:"hash"`
	Pinned  bool              `json:"pinned"`
	Bundle  string            `json:"bundle"`
	Renders []string          `json:"renders"`
	Lazy    []string          `json:"lazy"`
	Files   map[string]string `json:"files"`
}

// File is a vendored file: its name in the static files, and its SRI.
type File struct {
	Name      string
	Integrity string
}

// Set is the vendored components.
type Set struct {
	Lock   Lock
	bySlug map[string]*Component
}

// Load reads lock.json from the site's static files.
func Load(static fs.FS) (*Set, error) {
	b, err := fs.ReadFile(static, Dir+"/lock.json")
	if err != nil {
		return nil, err
	}
	s := &Set{bySlug: map[string]*Component{}}
	if err := json.Unmarshal(b, &s.Lock); err != nil {
		return nil, fmt.Errorf("starbase: lock.json: %w", err)
	}
	for i := range s.Lock.Components {
		c := &s.Lock.Components[i]
		s.bySlug[c.Slug] = c
	}
	return s, nil
}

// Has reports whether the component is vendored.
func (s *Set) Has(slug string) bool { return s.bySlug[slug] != nil }

// Scripts returns the bundles to load for the components: each one's, and
// those of the components it renders, these first, each once.
func (s *Set) Scripts(slugs ...string) ([]File, error) {
	var out []File
	seen := map[string]bool{}
	var add func(slug string) error
	add = func(slug string) error {
		if seen[slug] {
			return nil
		}
		seen[slug] = true
		c := s.bySlug[slug]
		if c == nil {
			return fmt.Errorf("starbase: %s is not vendored: add it to %s/pins.json and run go run ./cmd/vendor-starbase", slug, Dir)
		}
		for _, d := range c.Renders {
			if err := add(d); err != nil {
				return err
			}
		}
		out = append(out, File{Name: Dir + "/" + slug + "/" + c.Bundle, Integrity: c.Files[c.Bundle]})
		return nil
	}
	for _, slug := range slugs {
		if err := add(slug); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Lazy returns the files the bundles load on first use (ECharts' library),
// for the import map's integrity.
func (s *Set) Lazy() []File {
	var out []File
	for _, c := range s.Lock.Components {
		for _, p := range c.Lazy {
			out = append(out, File{Name: Dir + "/" + c.Slug + "/" + p, Integrity: c.Files[p]})
		}
	}
	slices.SortFunc(out, func(a, b File) int {
		if a.Name < b.Name {
			return -1
		}
		return 1
	})
	return out
}
