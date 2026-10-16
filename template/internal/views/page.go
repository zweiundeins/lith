package views

import (
	"encoding/json"

	"github.com/a-h/templ"
)

// importMap maps 'datastar' to Datastar's URL, for the modules that import
// it (newest-frame.js), with the request's CSP nonce.
func importMap(p Page) templ.Component {
	b, _ := json.Marshal(map[string]any{"imports": map[string]string{"datastar": p.JS}}) // escapes <, > and &: safe in a <script>
	return templ.Raw(`<script type="importmap" nonce="` + p.Nonce + `">` + string(b) + `</script>`)
}
