// Package githistory says when each page of the site last changed: the
// newest commit of the files it is made of. The binary has no git, so
// cmd/githistory (go tool task history, which build, dev and test run
// first) writes data/history.json from git log, and the binary embeds it.
// The file is not in the repository: without it, in a fresh checkout, every
// page simply has no history.
package githistory

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/zweiundeins/lith/site/internal/docs"
)

// Commit is the last commit that changed a page.
type Commit struct {
	Hash    string    `json:"hash"`
	Date    time.Time `json:"date"`
	Subject string    `json:"subject"`
}

// File is where cmd/githistory writes the history, relative to the site's
// module.
const File = "internal/githistory/data/history.json"

//go:embed all:data
var data embed.FS

// Load returns the embedded history, by page path ("/", "/demos",
// "/docs/concepts/ticks"). It is empty when the file was not generated.
func Load() (map[string]Commit, error) {
	b, err := fs.ReadFile(data, path.Base(path.Dir(File))+"/"+path.Base(File))
	if err != nil {
		return map[string]Commit{}, nil
	}
	h := map[string]Commit{}
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, fmt.Errorf("githistory: %w", err)
	}
	return h, nil
}

// Sources lists the files each page is made of, relative to the site's
// module, by page path: every docs page's Markdown file, every demo's
// package and the view around it, the demos' index and the landing page.
func Sources(d *docs.Docs, demos []string) map[string][]string {
	src := map[string][]string{
		"/":      {"internal/views/landing.templ", "internal/views/landing.go", "internal/web/static/css/components/landing.css", "internal/demos"},
		"/demos": {"internal/views/demo.templ", "internal/demos/demos.go"},
	}
	for _, slug := range demos {
		src["/demos/"+slug] = []string{"internal/demos/" + slug, "internal/views/demo.templ"}
	}
	for _, p := range d.Order {
		src[p.Path] = []string{"content/docs/" + p.File}
	}
	return src
}

// Collect runs git log in dir, the site's module, for each page's sources,
// and returns the newest commit of each. A page whose files have no
// commit yet is left out.
func Collect(dir string, sources map[string][]string) (map[string]Commit, error) {
	h := map[string]Commit{}
	for page, files := range sources {
		cmd := exec.Command("git", append([]string{"log", "-1", "--no-merges", "--format=%H%x00%cI%x00%s", "--"}, files...)...)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("githistory: git log for %s: %w", page, err)
		}
		line := strings.TrimSpace(string(out))
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\x00", 3)
		if len(f) != 3 {
			return nil, fmt.Errorf("githistory: git log for %s: %q", page, line)
		}
		date, err := time.Parse(time.RFC3339, f[1])
		if err != nil {
			return nil, fmt.Errorf("githistory: git log for %s: %w", page, err)
		}
		h[page] = Commit{Hash: f[0], Date: date, Subject: f[2]}
	}
	return h, nil
}
