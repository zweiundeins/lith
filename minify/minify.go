// Package minify minifies JavaScript and CSS for edge.Static: JavaScript
// with esbuild, CSS with tdewolff/minify. Passed to edge.Minify, Bytes
// minifies the scripts and stylesheets that Static reads and those that Add
// gives it, before Static names and precompresses them:
//
//	static, err := edge.NewStatic(files, "/static/", edge.Minify(minify.Bytes))
//
// It is a module of its own, so that projects that serve their files as
// written never pull in the minifiers. Leave the option out in development,
// so that the browser's devtools show the files as written.
package minify

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/css"
)

var (
	// The ES2020 target keeps esbuild from writing a || (a = b) as a ||= b.
	// Supported lets newer syntax that an author writes through, instead of
	// lowering class fields to helpers or failing on top-level await.
	scripts = api.TransformOptions{
		Loader:            api.LoaderJS,
		Target:            api.ES2020,
		MinifyWhitespace:  true,
		MinifySyntax:      true,
		MinifyIdentifiers: true,
		Charset:           api.CharsetUTF8,
		LegalComments:     api.LegalCommentsInline,
		Supported: map[string]bool{
			"arbitrary-module-namespace-names": true,
			"class-field":                      true,
			"class-private-accessor":           true,
			"class-private-brand-check":        true,
			"class-private-field":              true,
			"class-private-method":             true,
			"class-private-static-accessor":    true,
			"class-private-static-field":       true,
			"class-private-static-method":      true,
			"class-static-blocks":              true,
			"class-static-field":               true,
			"decorators":                       true,
			"hashbang":                         true,
			"import-attributes":                true,
			"regexp-match-indices":             true,
			"regexp-set-notation":              true,
			"top-level-await":                  true,
			"using":                            true,
		},
	}
	styles = &css.Minifier{}
	// m is what the CSS minifier minifies a data: URI with: a data URI of
	// a type it does not hold stays as it is.
	m = minify.New()
)

func init() {
	m.Add("text/css", styles)
}

// Bytes returns b minified: JavaScript, scripts and modules alike, for the
// media type text/javascript, and CSS for text/css; another type is an
// error. Comments and the spaces a file can do without go, and local names
// get shorter; comments that start with /*! or hold @license or @preserve
// stay. A file with import or export is a module, whose top-level names are
// local too: what it imports and exports keeps its names. A file without is
// a classic script, whose top-level names are globals that other scripts
// may use, so they keep theirs. Strings and template literals keep their
// text (quotes may change, a string to a template literal and back), and a
// tagged template's text stays byte for byte, as its tag sees it.
// JavaScript that does not parse is an error that names its line and
// column. Nothing esbuild writes of its own is newer than ES2020: newer
// syntax an author writes stays, except that a ||= b and its kin come out
// as a || (a = b). CSS keeps its layers, nesting and container queries,
// and at-rules it does not know, such as @container and @scope, keep their
// blocks as written; a declaration it cannot parse is copied as it is, as
// browsers skip it.
//
// It has the signature of edge.Minify's function, and of tdewolff's
// (*minify.M).Bytes, which an app with its own minify.M passes instead.
func Bytes(mediatype string, b []byte) ([]byte, error) {
	mediatype, _, _ = strings.Cut(mediatype, ";")
	switch strings.ToLower(strings.TrimSpace(mediatype)) {
	case "text/javascript", "application/javascript":
		res := api.Transform(string(b), scripts)
		if len(res.Errors) > 0 {
			e := res.Errors[0]
			if l := e.Location; l != nil {
				return nil, fmt.Errorf("minify: %s: line %d, column %d: %s", mediatype, l.Line, l.Column+1, e.Text)
			}
			return nil, fmt.Errorf("minify: %s: %s", mediatype, e.Text)
		}
		return bytes.TrimSuffix(res.Code, []byte("\n")), nil
	case "text/css":
		var out bytes.Buffer
		if err := styles.Minify(m, &out, bytes.NewReader(b), nil); err != nil {
			return nil, fmt.Errorf("minify: %s: %w", mediatype, err)
		}
		return out.Bytes(), nil
	default:
		return nil, fmt.Errorf("minify: no minifier for %q", mediatype)
	}
}
