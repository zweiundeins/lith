package sse

import (
	"bytes"
	"errors"
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
	elementsEvent  = "event: datastar-patch-elements\n"
	signalsEvent   = "event: datastar-patch-signals\n"
	elementsPrefix = "elements "
)

// PatchElements formats a datastar-patch-elements event. It takes at most
// one Patch; without one, the elements are morphed in by their ids.
func PatchElements(html []byte, p ...Patch) []byte {
	return AppendPatchElements(nil, html, p...)
}

// AppendPatchElements is PatchElements that appends the event to dst and
// returns the extended slice, so that a Render can format its frame into
// a buffer of the size it needs (live.FrameBuffer), or format several
// events into one.
func AppendPatchElements(dst, html []byte, p ...Patch) []byte {
	var opts [5]line
	return appendFormat(dst, elementsEvent, patchLines(&opts, p), elementsPrefix, html)
}

// patchLines are the option lines of a Patch, in opts.
func patchLines(opts *[5]line, p []Patch) []line {
	var o Patch
	if len(p) > 0 {
		o = p[0]
	}
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
	return opts[:n]
}

// PatchSignals formats a datastar-patch-signals event with a JSON merge
// patch. With onlyIfMissing, the page keeps signals it already has.
func PatchSignals(json []byte, onlyIfMissing bool) []byte {
	if onlyIfMissing {
		return appendFormat(nil, signalsEvent, []line{{"onlyIfMissing ", "true"}}, "signals ", json)
	}
	return appendFormat(nil, signalsEvent, nil, "signals ", json)
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

// appendFormat appends an event to dst: its options, then one data line
// per line of body, each with the given prefix. It grows dst once, and a
// nil dst to the event's exact size.
func appendFormat(dst []byte, event string, opts []line, prefix string, body []byte) []byte {
	n := len(event) + 1
	for _, o := range opts {
		n += len("data: ") + len(o.key) + len(o.value) + 1
	}
	if len(body) > 0 {
		for l := range lines(body) {
			n += len("data: ") + len(prefix) + len(l) + 1
		}
	}
	if cap(dst)-len(dst) < n {
		b := make([]byte, len(dst), len(dst)+n)
		copy(b, dst)
		dst = b
	}
	dst = appendHead(dst, event, opts)
	if len(body) > 0 {
		for l := range lines(body) {
			dst = append(dst, "data: "...)
			dst = append(dst, prefix...)
			dst = append(dst, l...)
			dst = append(dst, '\n')
		}
	}
	return append(dst, '\n')
}

// appendHead appends an event's name and its option lines to dst.
func appendHead(dst []byte, event string, opts []line) []byte {
	dst = append(dst, event...)
	for _, o := range opts {
		dst = append(dst, "data: "...)
		dst = append(dst, o.key...)
		dst = append(dst, o.value...)
		dst = append(dst, '\n')
	}
	return dst
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

// Elements formats a datastar-patch-elements event from HTML written to it
// in pieces, such as by a template's Render: each piece goes into the
// event as it comes, line by line, so the event holds the only copy of the
// HTML. Its lines end as PatchElements ends them, at "\r\n", "\r" or "\n",
// also where a write ends between a CR and its LF.
//
//	e := sse.NewElements(live.FrameBuffer(ctx))
//	if err := page.Render(ctx, e); err != nil {
//		return nil, err
//	}
//	return e.Event(), nil
type Elements struct {
	b     []byte
	html  bool // HTML was written
	open  bool // a data line has begun
	cr    bool // the last byte written was a CR
	ended bool
}

// NewElements starts an event in dst, which Event returns extended by it.
// It takes at most one Patch, as PatchElements does.
func NewElements(dst []byte, p ...Patch) *Elements {
	var opts [5]line
	return &Elements{b: appendHead(dst, elementsEvent, patchLines(&opts, p))}
}

// errEnded is the error of a write to Elements after its Event.
var errEnded = errors.New("sse: Elements written after its Event")

// Write adds HTML to the event.
func (e *Elements) Write(p []byte) (int, error) {
	if e.ended {
		return 0, errEnded
	}
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	e.html = true
	if e.cr && p[0] == '\n' {
		p = p[1:]
	}
	e.cr = false
	cr := bytes.IndexByte(p, '\r') >= 0
	for len(p) > 0 {
		if !e.open {
			e.b = append(e.b, "data: "+elementsPrefix...)
			e.open = true
		}
		var i int
		if cr {
			i = bytes.IndexAny(p, "\r\n")
		} else {
			i = bytes.IndexByte(p, '\n')
		}
		if i < 0 {
			e.b = append(e.b, p...)
			break
		}
		e.b = append(append(e.b, p[:i]...), '\n')
		e.open = false
		if p[i] == '\r' {
			switch {
			case i+1 == len(p):
				e.cr = true
			case p[i+1] == '\n':
				i++
			}
		}
		p = p[i+1:]
	}
	return n, nil
}

// Event ends the event and returns dst with it. Later calls return the
// same, and writes fail.
func (e *Elements) Event() []byte {
	if !e.ended {
		e.ended = true
		switch {
		case e.open:
			e.b = append(e.b, '\n')
		case e.html:
			// The HTML ended a line: an empty one follows, as PatchElements has it.
			e.b = append(e.b, "data: "+elementsPrefix+"\n"...)
		}
		e.b = append(e.b, '\n')
	}
	return e.b
}
