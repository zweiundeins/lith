package views

import (
	"github.com/a-h/templ"

	"github.com/zweiundeins/lith/site/internal/markdown"
)

// Landing is what the landing page shows besides its fixed copy.
type Landing struct {
	// Slots are the embeds of the demos that fill the landing page's slots,
	// by slot ("hero", "linear", "immediate", "ticks", "hypermedia"), each
	// already wrapped in its demo's data-demo element.
	Slots map[string]templ.Component
	Cards []Card
	Repo  string
}

// Card is a demo on the landing page and on /demos.
type Card struct {
	Slug, Title, Summary string
	Art                  string // SVG markup
}

// idea is one letter of the name.
type idea struct {
	Letter, Name, Slot, Href string
	Text                     string
	// Listing shows beside the idea while no demo fills its slot.
	Listing, Lang, ListingTitle string
}

var ideas = []idea{
	{
		Letter: "L", Name: "Linear writes", Slot: "linear", Href: "/docs/concepts/linear-writes",
		Text: "Every change is a command, and one writer applies them in the order they arrive. " +
			"It takes all the commands waiting into one transaction, with a savepoint each: a command that fails rolls back alone, " +
			"and the rest commit together. The more people write at once, the more commands share each commit.",
		Lang: "go", ListingTitle: "a command",
		Listing: `type Sign struct{ Name, Message string }

func (c Sign) Apply(tx *writer.Tx) error {
	_, err := tx.Exec(` + "`INSERT INTO entries (name, message)\n\t\tVALUES (?, ?)`" + `, c.Name, c.Message)
	tx.Touch("guestbook") // its views render again
	return err
}

// The handler waits for the commit, answers 204,
// and the page learns the outcome from its stream.
err := w.Do(r.Context(), Sign{Name: name, Message: msg})`,
	},
	{
		Letter: "I", Name: "Immediate mode", Slot: "immediate", Href: "/docs/concepts/immediate-mode",
		Text: "A region of a page is a function of the database: view = f(state). " +
			"The server renders it whole, in one read transaction, and Datastar morphs the new HTML into the page. " +
			"No state in the browser guesses what the server will say, so there is nothing to reconcile.",
		Lang: "go", ListingTitle: "a view",
		Listing: `func entries(ctx context.Context, tx *sql.Tx) (any, error) {
	list, err := latest(ctx, tx, 20) // one snapshot for the whole region
	if err != nil {
		return nil, err
	}
	return patch(ctx, views.Entries(list)) // the whole region, every time
}`,
	},
	{
		Letter: "T", Name: "Ticks", Slot: "ticks", Href: "/docs/concepts/ticks",
		Text: "A commit names the topics it touched, and every view that listens to one of them turns dirty. " +
			"Once per tick, 50 ms by default, each dirty view renders once and its frame goes to every stream that watches it: " +
			"two hundred tabs on one board cost one render.",
		Lang: "go", ListingTitle: "a shared view",
		Listing: `live.View{
	Key:    "board:42",           // everyone on board 42 shares it
	Topics: []string{"board:42"}, // a commit that touches it marks it dirty
	Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
		return renderBoard(ctx, tx, 42) // once a tick, for all
	},
}`,
	},
	{
		Letter: "H", Name: "Hypermedia", Slot: "hypermedia", Href: "/docs/concepts/hypermedia",
		Text: "Frames reach the browser as Datastar events over server-sent events. " +
			"Each stream compresses with a window of its own, so a frame that repeats most of the last one costs a few hundred bytes, " +
			"and a memory budget shrinks the windows as more streams open.",
		Lang: "text", ListingTitle: "a frame on the wire, before zstd",
		Listing: `event: datastar-patch-elements
data: elements <section id="entries">
data: elements   <h2>213 entries</h2>
data: elements   <ul><li id="entry-213">Hello from Biel</li>…</ul>
data: elements </section>`,
	},
}

func (i idea) listing() templ.Component {
	return templ.Raw(markdown.CodeBlock(i.Listing, i.Lang, i.ListingTitle))
}

// stat is one measured number.
type stat struct {
	Value, Label, Detail string
}

var stats = []stat{
	{"66,688", "commands a second through one writer", "200 producers, 109 commands to a transaction, synchronous=NORMAL"},
	{"0.54 ms", "from a command's arrival to its commit, at p50", "beside 200 open streams and 800 commands a second"},
	{"1", "render per tick for 200 streams of one board", "a 139 KB view changing 800 times a second, no frame dropped"},
	{"2,205", "bytes on the wire for each frame of that board", "zstd, every stream compressed on its own"},
	{"1.0 MiB", "of heap for an open stream", "zstd with a 2^19 window and low memory; brotli at its default window holds 9.9 MiB"},
	{"14,928", "moves a second in kanban, with 200 people", "a move answered in 13.1 ms at p95"},
}

const quickstart = `go run github.com/zweiundeins/lith/cmd/lith-new@latest \
	~/guestbook example.com/you/guestbook
cd ~/guestbook && go mod tidy
go tool task dev   # http://127.0.0.1:8080`

func quickstartCode() templ.Component {
	return templ.Raw(markdown.CodeBlock(quickstart, "sh", "shell"))
}

// embers are the sparks that drift up from the hero's monolith.
var embers = []struct{ X, Delay, Dur, Drift string }{
	{"-18px", "0s", "7s", "10px"}, {"6px", "1.8s", "8.5s", "-14px"}, {"-4px", "3.1s", "6.5s", "18px"},
	{"14px", "4.4s", "9s", "-6px"}, {"-24px", "5.6s", "7.5s", "-12px"}, {"22px", "2.4s", "8s", "8px"},
}

func emberStyle(x, delay, dur, drift string) string {
	return "--x: " + x + "; --delay: " + delay + "; --d: " + dur + "; --drift: " + drift
}
