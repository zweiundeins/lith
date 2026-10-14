package gen

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
)

// includeBlock finds a part of a page that a file of lith's repository
// fills: the lines between <!-- include: <path> --> and <!-- end include -->,
// such as a table that a benchmark writes.
var includeBlock = regexp.MustCompile(`(?s)(<!-- include: (\S+) -->\n)(.*?)(<!-- end include -->)`)

// include fills the page's included parts from their files, relative to
// root. It reports whether the page has any.
func include(root, page string) (string, bool, error) {
	found := false
	var errs []error
	out := includeBlock.ReplaceAllStringFunc(page, func(block string) string {
		m := includeBlock.FindStringSubmatch(block)
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(m[2])))
		if err != nil {
			errs = append(errs, err)
			return block
		}
		found = true
		return m[1] + string(b) + m[4]
	})
	return out, found, errors.Join(errs...)
}
