package views

import "github.com/a-h/templ"

// icons are small pixel icons, drawn on a grid of 16 with crisp edges and
// filled with currentColor.
var icons = map[string]string{
	// a sun: a disc and eight rays
	"sun": `<path d="M7 1h2v2H7zM7 13h2v2H7zM1 7h2v2H1zM13 7h2v2h-2zM3 3h2v2H3zM11 3h2v2h-2zM3 11h2v2H3zM11 11h2v2h-2zM6 5h4v1h1v4h-1v1H6v-1H5V6h1z"/>`,
	// a crescent
	"moon": `<path d="M6 2h4v1H8v1H7v2H6v4h1v2h1v1h2v1H6v-1H4v-1H3v-2H2V6h1V4h1V3h2z"/>`,
	// a half-filled square: the system's choice
	"system": `<path d="M2 3h12v10H2zM3 4v8h5V4z"/>`,
	// sharp pixel corners and smooth ones
	"pixel":  `<path d="M3 2h10v1h1v10h-1v1H3v-1H2V3h1zM4 4v8h8V4z"/>`,
	"smooth": `<path d="M4 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2zm0 2v8h8V4z"/>`,
	// an arrow to the right and to the left
	"arrow-right": `<path d="M2 7h8V5h1v1h1v1h1v2h-1v1h-1v1h-1V9H2z"/>`,
	"arrow-left":  `<path d="M14 7H6V5H5v1H4v1H3v2h1v1h1v1h1V9h8z"/>`,
	// three bars
	"menu": `<path d="M2 3h12v2H2zM2 7h12v2H2zM2 11h12v2H2z"/>`,
	// the display menu's button: a sun half in shade
	"display": `<path d="M7 1h2v2H7zM7 13h2v2H7zM1 7h2v2H1zM13 7h2v2h-2zM3 3h2v2H3zM11 3h2v2h-2zM3 11h2v2H3zM11 11h2v2h-2zM6 5h4v1h1v4h-1v1H6v-1H5V6h1zM8 6v4h1V9h1V7H9V6z"/>`,
	// GitHub's mark
	"github": `<path d="M8 .2a8 8 0 0 0-2.53 15.59c.4.07.55-.17.55-.38v-1.33c-2.23.48-2.7-1.07-2.7-1.07-.36-.92-.89-1.17-.89-1.17-.73-.5.06-.49.06-.49.8.06 1.23.83 1.23.83.71 1.23 1.87.87 2.33.67.07-.52.28-.87.5-1.07-1.78-.2-3.65-.89-3.65-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.6 7.6 0 0 1 4 0c1.53-1.03 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.28.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48v2.2c0 .21.15.46.55.38A8 8 0 0 0 8 .2z"/>`,
}

// Icon is a pixel icon by name, decorative (aria-hidden).
func Icon(name string) templ.Component {
	shape := "crispEdges"
	if name == "github" || name == "smooth" {
		shape = "geometricPrecision"
	}
	return templ.Raw(`<svg class="icon" viewBox="0 0 16 16" fill="currentColor" shape-rendering="` + shape + `" aria-hidden="true" focusable="false">` + icons[name] + `</svg>`)
}
