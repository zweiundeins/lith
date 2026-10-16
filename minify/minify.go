// Package minify minifies JavaScript and CSS for edge.Static, with
// tdewolff/minify. Passed to edge.Minify, Bytes minifies the scripts and
// stylesheets that Static reads and those that Add gives it, before Static
// names and precompresses them:
//
//	static, err := edge.NewStatic(files, "/static/", edge.Minify(minify.Bytes))
//
// It is a module of its own, so that projects that serve their files as
// written never pull in the minifier. Leave the option out in development,
// so that the browser's devtools show the files as written.
package minify

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/tdewolff/minify/v2/css"
	"github.com/tdewolff/minify/v2/js"
)

var (
	// No syntax newer than ES2020 in what it writes, whatever tdewolff
	// learns later.
	scripts = &js.Minifier{Version: 2020}
	styles  = &css.Minifier{}
)

// Bytes returns b minified: JavaScript, scripts and modules alike, for the
// media type text/javascript, and CSS for text/css; another type is an
// error. Comments and the spaces a file can do without go, and local names
// get shorter. What a module imports and exports keeps its names, strings
// and template literals keep their text (quotes may change), and a tagged
// template's text stays byte for byte, as its tag sees it. JavaScript that
// does not parse is an error that names its line. CSS keeps its layers,
// nesting and container queries, and at-rules it does not know, such as
// @container and @scope, keep their blocks as written; a declaration it
// cannot parse is copied as it is, as browsers skip it.
//
// It has the signature of edge.Minify's function, and of tdewolff's
// (*minify.M).Bytes, which an app with its own minify.M passes instead.
func Bytes(mediatype string, b []byte) ([]byte, error) {
	mediatype, _, _ = strings.Cut(mediatype, ";")
	var out bytes.Buffer
	var err error
	switch strings.ToLower(strings.TrimSpace(mediatype)) {
	case "text/javascript", "application/javascript":
		err = scripts.Minify(nil, &out, bytes.NewReader(b), nil)
	case "text/css":
		err = styles.Minify(nil, &out, bytes.NewReader(b), nil)
	default:
		return nil, fmt.Errorf("minify: no minifier for %q", mediatype)
	}
	if err != nil {
		return nil, fmt.Errorf("minify: %s: %w", mediatype, err)
	}
	return out.Bytes(), nil
}
