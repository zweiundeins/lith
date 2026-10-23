package views

import (
	"encoding/json"

	"github.com/a-h/templ"
)

// What Update says.
const (
	UpdateTyped  = "This page has a new version. Reloading clears the form."
	UpdateAssets = "This page has a new version."
)

// importMap maps 'datastar' to Datastar's URL, for the modules that import
// it (newest-frame.js), with the request's CSP nonce.
func importMap(p Page) templ.Component {
	b, _ := json.Marshal(map[string]any{"imports": map[string]string{"datastar": p.JS}}) // escapes <, > and &: safe in a <script>
	return templ.Raw(`<script type="importmap" nonce="` + p.Nonce + `">` + string(b) + `</script>`)
}
