// Package stream is the stream inspector: a tab watches its own stream.
// Each write to the tab's stream carries a report read from the stream
// itself as the write goes out: the content coding, the tier the memory
// budget gave its encoder, the bytes of events and on the wire, the frames
// a second and the heartbeats, and from its budget the streams open on the
// site and the memory they reserve. A live table is the payload; its size
// and churn are the tab's state.
package stream

import (
	_ "embed"

	"github.com/zweiundeins/lith/site/internal/demo"
)

const Slug = "stream"

// TabState is a tab's state in site_tabs: the payload it picked, and the op
// id of its last change. A still table shows the step it froze at, with
// the churn it had before.
type TabState struct {
	Size   Size   `json:"size,omitempty"`
	Churn  Churn  `json:"churn,omitempty"`
	Frozen int64  `json:"frozen,omitempty"`
	From   Churn  `json:"from,omitempty"`
	Op     string `json:"op,omitempty"`
}

// size and churn are the tab's choices, or the defaults of a new tab.
func (s TabState) size() Size {
	if s.Size.valid() {
		return s.Size
	}
	return DefaultSize
}

func (s TabState) churn() Churn {
	if s.Churn.valid() {
		return s.Churn
	}
	return DefaultChurn
}

// from is the churn a still table had before it froze.
func (s TabState) from() Churn {
	if s.From.valid() && s.From != Still {
		return s.From
	}
	return DefaultChurn
}

var (
	//go:embed server.md
	server string
	//go:embed how.md
	how string
	//go:embed stream.css
	css string
)

// Demo is the stream inspector as the site lists it.
var Demo = demo.Demo{
	Slug:       Slug,
	Title:      "Stream inspector",
	Summary:    "Your own stream, live: what the hub sends this tab, what zstd makes of it, and what the memory budget gave its encoder.",
	Art:        art,
	Server:     server,
	How:        how,
	CSS:        css,
	Components: []string{"sparkline", "meter"},
	Mount:      mount,
	Page:       demo.Part{Render: renderPage, Views: pageViews},
	Embed:      &demo.Embed{Slot: demo.SlotHypermedia, Part: demo.Part{Render: renderEmbed, Views: embedViews}},
}
