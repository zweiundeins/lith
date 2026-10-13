// Package gen writes the parts of the docs that come from files outside
// the site's module, which go:embed cannot reach: the package reference,
// from the Go doc comments of lith's packages, the rows of the charts that
// plot a CSV file of lith's benchmarks, and the changelog. cmd/docgen runs it (go
// generate ./content), and TestUpToDate fails when the files it would
// write differ from those in content/docs.
package gen

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Module is lith's module path.
const Module = "github.com/zweiundeins/lith"

// Root returns lith's repository root: dir or the first directory above
// it whose go.mod declares Module.
func Root(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.HasPrefix(string(b), "module "+Module+"\n") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("gen: lith's repository root not found")
		}
		dir = parent
	}
}

// Docs is the docs' directory, relative to the root.
const Docs = "site/content/docs"

// Generate returns every file the generator writes, by its path relative
// to the root: the reference's pages, each page with a chart drawn from a
// CSV file, and the changelog's.
func Generate(root string) (map[string][]byte, error) {
	ref, err := Reference(root)
	if err != nil {
		return nil, err
	}
	charts, err := Charts(root)
	if err != nil {
		return nil, err
	}
	changelog, err := Changelog(root)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	maps.Copy(files, ref)
	maps.Copy(files, charts)
	maps.Copy(files, changelog)
	return files, nil
}

// Write writes the generated files under root and removes the reference
// pages it no longer generates. It returns the paths it changed.
func Write(root string, files map[string][]byte) ([]string, error) {
	var changed []string
	for _, path := range slices.Sorted(maps.Keys(files)) {
		abs := filepath.Join(root, path)
		if old, err := os.ReadFile(abs); err == nil && string(old) == string(files[path]) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return changed, err
		}
		if err := os.WriteFile(abs, files[path], 0o644); err != nil {
			return changed, err
		}
		changed = append(changed, path)
	}
	stale, err := Stale(root, files)
	if err != nil {
		return changed, err
	}
	for _, path := range stale {
		if err := os.Remove(filepath.Join(root, path)); err != nil {
			return changed, err
		}
		changed = append(changed, path)
	}
	return changed, nil
}

// Stale lists the files of the reference's directory that the generator
// does not write.
func Stale(root string, files map[string][]byte) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, refDir))
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, e := range entries {
		path := refDir + "/" + e.Name()
		if _, ok := files[path]; !ok {
			stale = append(stale, path)
		}
	}
	return stale, nil
}

// Diff lists the generated files that differ from those under root, and
// the stale ones, each with what is wrong.
func Diff(root string, files map[string][]byte) ([]string, error) {
	var diffs []string
	for _, path := range slices.Sorted(maps.Keys(files)) {
		old, err := os.ReadFile(filepath.Join(root, path))
		switch {
		case err != nil:
			diffs = append(diffs, fmt.Sprintf("%s: %v", path, err))
		case string(old) != string(files[path]):
			diffs = append(diffs, fmt.Sprintf("%s: differs from what the generator writes", path))
		}
	}
	stale, err := Stale(root, files)
	if err != nil {
		return nil, err
	}
	for _, path := range stale {
		diffs = append(diffs, fmt.Sprintf("%s: not generated any more", path))
	}
	return diffs, nil
}
