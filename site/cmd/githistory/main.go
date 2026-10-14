// Command githistory writes the date, hash and subject of the last commit
// of every page of the site to internal/githistory/data/history.json, for
// the docs' "Updated" line, their JSON-LD and the sitemap's lastmod. Run it
// in site/ (go tool task history); the build, dev and test tasks run it
// first.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zweiundeins/lith/site/content"
	"github.com/zweiundeins/lith/site/internal/demos"
	"github.com/zweiundeins/lith/site/internal/docs"
	"github.com/zweiundeins/lith/site/internal/githistory"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "githistory:", err)
		os.Exit(1)
	}
}

func run() error {
	d, err := docs.Load(content.Docs, true)
	if err != nil {
		return err
	}
	var slugs []string
	for _, x := range demos.All {
		slugs = append(slugs, x.Slug)
	}
	h, err := githistory.Collect(".", githistory.Sources(d, slugs))
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(h, "", "\t")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(githistory.File), 0o755); err != nil {
		return err
	}
	return os.WriteFile(githistory.File, append(b, '\n'), 0o644)
}
