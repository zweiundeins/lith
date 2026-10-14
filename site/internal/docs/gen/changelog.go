package gen

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// changelogPage is the docs' copy of CHANGELOG.md.
const changelogPage = Docs + "/changelog/index.md"

// Changelog returns the changelog's page: CHANGELOG.md from the root,
// under the docs' front matter, which gives the page its title in place of
// the file's own.
func Changelog(root string) (map[string][]byte, error) {
	b, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		return nil, err
	}
	body, ok := strings.CutPrefix(string(b), "# Changelog\n")
	if !ok {
		return nil, errors.New("gen: CHANGELOG.md does not start with \"# Changelog\"")
	}
	page := frontMatter("Changelog", "", "What each release of lith changed.", changelogMeta, 9) + "\n" + body
	return map[string][]byte{changelogPage: []byte(page)}, nil
}
