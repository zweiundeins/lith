package views

import (
	"encoding/json"

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
	Topics: []string{"board:42"}, // a commit to it marks it dirty
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
	{"80,530", "commands a second through one writer", "200 producers, 108 commands to a transaction, synchronous=NORMAL"},
	{"0.42 ms", "from a command's arrival to its commit, at p50", "beside 200 open streams and 800 commands a second"},
	{"1", "render per tick for 200 streams of one board", "a 139 KB view changing 800 times a second, on 0.55 of a core"},
	{"2,188", "bytes on the wire for each frame of that board", "of 142,067 bytes of HTML, 65 times smaller: zstd, every stream compressed on its own"},
	{"14,480", "commands a second with synchronous=FULL", "200 producers sharing each 7 ms fsync, 108 to a transaction"},
	{"1.7 MiB", "of heap for an open stream", "zstd with a 2^19 window; with low memory 1.0 MiB, brotli at its default window 9.9 MiB"},
}

// start is one way to begin, a tab of the quick start (Starbase's sb-tabs).
type start struct {
	Label, Slot, Text, Code string
}

var starts = []start{
	{"New project", "new-project", "A new module from lith's template, its guestbook running.",
		`go run github.com/zweiundeins/lith/cmd/lith-new@latest \
	~/guestbook example.com/you/guestbook
cd ~/guestbook && go mod tidy
go tool task dev   # http://127.0.0.1:8080`},
	{"Into a module", "into-a-module", "lith in a module of your own: then wire the database, the writer and the hub in main.",
		`go get github.com/zweiundeins/lith@latest`},
	{"From a checkout", "from-a-checkout", "A project on a lith checkout: its go.mod gets a replace directive for the checkout, for work on both.",
		`git clone https://github.com/zweiundeins/lith ~/src/lith
cd ~/src/lith && go run ./cmd/lith-new -lith . ~/guestbook example.com/you/guestbook`},
}

// startLabels are the tabs' labels, for sb-tabs' labels attribute.
func startLabels() string {
	b, _ := json.Marshal([]string{starts[0].Label, starts[1].Label, starts[2].Label})
	return string(b)
}

func (s start) code() templ.Component {
	return templ.Raw(markdown.CodeBlock(s.Code, "sh", "shell"))
}

// embers are the sparks that drift up from the hero's monolith.
var embers = []struct{ X, Delay, Dur, Drift string }{
	{"-18px", "0s", "7s", "10px"}, {"6px", "1.8s", "8.5s", "-14px"}, {"-4px", "3.1s", "6.5s", "18px"},
	{"14px", "4.4s", "9s", "-6px"}, {"-24px", "5.6s", "7.5s", "-12px"}, {"22px", "2.4s", "8s", "8px"},
}

func emberStyle(x, delay, dur, drift string) string {
	return "--x: " + x + "; --delay: " + delay + "; --d: " + dur + "; --drift: " + drift
}
