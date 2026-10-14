package sse

import (
	"bytes"
	"html"
	"iter"
	"strconv"
)

// Mode is how patched elements are applied to the page.
type Mode string

// The patch modes of Datastar v1.
const (
	Outer   Mode = "outer" // morph the element with the same id (the default)
	Inner   Mode = "inner" // morph the selected element's children
	Replace Mode = "replace"
	Prepend Mode = "prepend"
	Append  Mode = "append"
	Before  Mode = "before"
	After   Mode = "after"
	Remove  Mode = "remove"
)

// Namespace is the namespace of patched elements that are not HTML.
type Namespace string

// The namespaces Datastar v1 knows besides HTML.
const (
	SVG    Namespace = "svg"
	MathML Namespace = "mathml"
)

// Patch says where and how patched elements go. The zero value morphs
// each element into the page by its id. Values must be single lines.
//
// ViewTransition morphs inside a view transition, the whole document's
// unless ViewTransitionSelector names an element to scope it to. Scope
// it: while a transition of the document runs, Chromium gives every press
// on the page to <html>, and buttons and links miss their clicks. A
// browser without scoped transitions morphs at once.
type Patch struct {
	Selector               string
	Mode                   Mode
	Namespace              Namespace
	ViewTransition         bool
	ViewTransitionSelector string // used only with ViewTransition
}

const (
	elementsEvent = "event: datastar-patch-elements\n"
	signalsEvent  = "event: datastar-patch-signals\n"
)

// PatchElements formats a datastar-patch-elements event. It takes at most
// one Patch; without one, the elements are morphed in by their ids.
func PatchElements(html []byte, p ...Patch) []byte {
	var o Patch
	if len(p) > 0 {
		o = p[0]
	}
	var opts [5]line
	n := 0
	if o.Selector != "" {
		opts[n] = line{"selector ", o.Selector}
		n++
	}
	if o.Mode != "" && o.Mode != Outer {
		opts[n] = line{"mode ", string(o.Mode)}
		n++
	}
	if o.Namespace != "" && o.Namespace != "html" {
		opts[n] = line{"namespace ", string(o.Namespace)}
		n++
	}
	if o.ViewTransition {
		opts[n] = line{"useViewTransition ", "true"}
		n++
		if o.ViewTransitionSelector != "" {
			opts[n] = line{"viewTransitionSelector ", o.ViewTransitionSelector}
			n++
		}
	}
	return format(elementsEvent, opts[:n], "elements ", html)
}

// PatchSignals formats a datastar-patch-signals event with a JSON merge
// patch. With onlyIfMissing, the page keeps signals it already has.
func PatchSignals(json []byte, onlyIfMissing bool) []byte {
	if onlyIfMissing {
		return format(signalsEvent, []line{{"onlyIfMissing ", "true"}}, "signals ", json)
	}
	return format(signalsEvent, nil, "signals ", json)
}

// Reconnect is the last event of a stream that ends because its server
// restarts. It appends to the body an element that replaces the element
// with the given id by a copy of it, so that the copy's data-init, the
// @get or @post that opened the stream, runs again with its own options.
// That request must use retry 'error': Datastar 1.0.4 retries it while the
// restart answers 502 or 503, where the default 'auto' gives up. A stream
// that ends cleanly without Reconnect stays closed.
func Reconnect(id string) []byte {
	el := "document.getElementById(" + strconv.Quote(id) + ")"
	js := "el.remove(); " + el + "?.replaceWith(" + el + ".cloneNode(true))"
	return PatchElements([]byte(`<div data-init="`+html.EscapeString(js)+`"></div>`), Patch{Selector: "body", Mode: Append})
}

// Comment formats an SSE comment, which the page ignores (a heartbeat).
func Comment(text string) []byte {
	b := make([]byte, 0, len(text)+4)
	for l := range lines([]byte(text)) {
		b = append(b, ':')
		if len(l) > 0 {
			b = append(b, ' ')
			b = append(b, l...)
		}
		b = append(b, '\n')
	}
	return append(b, '\n')
}

type line struct{ key, value string }

// format writes an event: its options, then one data line per line of
// body, each with the given prefix.
func format(event string, opts []line, prefix string, body []byte) []byte {
	n := len(event) + 1
	for _, o := range opts {
		n += len("data: ") + len(o.key) + len(o.value) + 1
	}
	if len(body) > 0 {
		for l := range lines(body) {
			n += len("data: ") + len(prefix) + len(l) + 1
		}
	}
	b := make([]byte, 0, n)
	b = append(b, event...)
	for _, o := range opts {
		b = append(b, "data: "...)
		b = append(b, o.key...)
		b = append(b, o.value...)
		b = append(b, '\n')
	}
	if len(body) > 0 {
		for l := range lines(body) {
			b = append(b, "data: "...)
			b = append(b, prefix...)
			b = append(b, l...)
			b = append(b, '\n')
		}
	}
	return append(b, '\n')
}

// lines yields the lines of b. Like an SSE parser, it ends a line at
// "\r\n", "\r" or "\n", so a stray carriage return cannot end a data
// line early.
func lines(b []byte) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		if bytes.IndexByte(b, '\r') < 0 {
			// Without a CR, IndexByte jumps from line to line far faster than a loop.
			for rest := b; ; {
				i := bytes.IndexByte(rest, '\n')
				if i < 0 {
					yield(rest)
					return
				}
				if !yield(rest[:i]) {
					return
				}
				rest = rest[i+1:]
			}
		}
		start := 0
		for i := 0; i < len(b); i++ {
			if c := b[i]; c == '\n' || c == '\r' {
				if !yield(b[start:i]) {
					return
				}
				if c == '\r' && i+1 < len(b) && b[i+1] == '\n' {
					i++
				}
				start = i + 1
			}
		}
		yield(b[start:])
	}
}
