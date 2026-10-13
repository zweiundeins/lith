package stream

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zweiundeins/lith/sse"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// probe is the frame of the report views: it reports on the stream that
// carries it. On the demo's page every frame brings a report; in the
// landing page's slot one a second at most. A stream that falls quiet gets
// a report of its last frame alone.
type probe struct{ embed bool }

// quiet is how long a stream sends no frame before its last frame gets a
// report of its own.
const quiet = 500 * time.Millisecond

// Report reads the stream as the site's frame writer is about to write to
// it: what it sent before this write, and its last frame.
func (p probe) Report(st *sse.Stream, m *demo.Meter, frames int) []byte {
	now := time.Now()
	switch {
	case frames > 0 && (!p.embed || now.Sub(m.ReportedAt) >= time.Second):
	case frames == 0 && m.Frames > m.Reported && now.Sub(m.Last.At) >= quiet:
	default:
		return nil
	}
	m.Reported, m.ReportedAt = m.Frames, now
	r := read(st, m, now)
	var v any = pageReport{r.page()}
	if p.embed {
		v = embedReport{r.embed()}
	}
	b, err := demo.Signals(v)
	if err != nil {
		return nil
	}
	return b
}

// Reading is what a report tells, read from the stream, its meter and its
// budget.
type Reading struct {
	Frames       int64      // frames the stream sent
	Coding       string     // sse.Zstd, sse.Brotli, sse.Gzip or ""
	Tier         string     // what its encoder holds, as the budget names it
	Heap         int64      // what the budget reserves for the encoder
	Events, Wire int64      // bytes of events, and on the wire
	Last         demo.Write // the last frame
	FPS          float64    // frames a second over the last four seconds
	Beats        int64      // heartbeats
	Budget       sse.BudgetStats
}

func read(st *sse.Stream, m *demo.Meter, now time.Time) Reading {
	r := Reading{Frames: m.Frames, Coding: st.Encoding(), Last: m.Last, FPS: m.Rate(now, 4*time.Second), Beats: m.Beats}
	r.Tier, r.Heap = st.Tier()
	r.Events, r.Wire = st.Sent()
	if b := st.Budget(); b != nil {
		r.Budget = b.Stats()
	}
	return r
}

// The signals of a report, preformatted: a report goes out with every
// frame, and text that has not changed costs next to nothing compressed.
type (
	pageReport struct {
		S PageReport `json:"_stream"`
	}
	embedReport struct {
		S EmbedReport `json:"_stream"`
	}
)

// PageReport is the demo page's report, $_stream.
type PageReport struct {
	N        int64   `json:"n"`        // frames before this one
	Coding   string  `json:"coding"`   // "zstd", "brotli", "gzip", "none"
	Tier     string  `json:"tier"`     // "zstd 2^19", "uncompressed"
	Heap     string  `json:"heap"`     // "1.66 MiB"
	Events   string  `json:"events"`   // bytes of events so far
	Wire     string  `json:"wire"`     // bytes on the wire so far
	Total    string  `json:"total"`    // their ratio, "69:1"
	FE       string  `json:"fe"`       // the last frame: bytes of events
	FW       int64   `json:"fw"`       // the last frame: bytes on the wire
	FWT      string  `json:"fwt"`      // the same, formatted
	FR       int64   `json:"fr"`       // the last frame: its ratio in tenths, 691 for 69.1:1
	Ratio    string  `json:"ratio"`    // the same, formatted
	FP       float64 `json:"fp"`       // the last frame: wire in percent of its events
	FPS      string  `json:"fps"`      // "4.0"
	Beats    int64   `json:"beats"`    // heartbeats
	Streams  int     `json:"streams"`  // open on the site
	Reserved string  `json:"reserved"` // by the budget for their encoders
	Limit    string  `json:"limit"`    // the budget's limit, "none"
	Budget   int     `json:"budget"`   // reserved in percent of the limit, -1 without one
	Tiers    string  `json:"tiers"`    // what the open streams hold
	Quiet    int     `json:"quiet"`    // 0: the page counts the seconds after it
}

// EmbedReport is the landing page slot's report.
type EmbedReport struct {
	N      int64  `json:"n"`
	Coding string `json:"coding"`
	Tier   string `json:"tier"`
	Total  string `json:"total"`
	FW     int64  `json:"fw"`
	FWT    string `json:"fwt"`
}

func (r Reading) page() PageReport {
	p := PageReport{
		N: r.Frames, Coding: codingName(r.Coding), Tier: tierName(r.Tier), Heap: mibText(r.Heap),
		Events: byteText(r.Events), Wire: byteText(r.Wire), Total: ratioText(r.Events, r.Wire),
		FE: byteText(r.Last.Events), FW: r.Last.Wire, FWT: byteText(r.Last.Wire),
		FPS: strconv.FormatFloat(r.FPS, 'f', 1, 64), Beats: r.Beats,
		Reserved: mibText(r.Budget.Reserved), Limit: mibText(r.Budget.Limit), Budget: -1,
		Tiers: tiersText(r.Budget.Streams),
	}
	if r.Last.Wire > 0 {
		p.FR = max(10, int64(math.Round(10*float64(r.Last.Events)/float64(r.Last.Wire))))
		p.FP = math.Round(1e4*float64(r.Last.Wire)/float64(max(r.Last.Events, r.Last.Wire))) / 100
	}
	p.Ratio = ratioText(r.Last.Events, r.Last.Wire)
	for _, n := range r.Budget.Streams {
		p.Streams += n
	}
	if r.Budget.Limit > 0 {
		p.Budget = int(math.Round(100 * float64(r.Budget.Reserved) / float64(r.Budget.Limit)))
	}
	return p
}

func (r Reading) embed() EmbedReport {
	return EmbedReport{N: r.Frames, Coding: codingName(r.Coding), Tier: tierName(r.Tier), Total: ratioText(r.Events, r.Wire), FW: r.Last.Wire, FWT: byteText(r.Last.Wire)}
}

func codingName(c string) string {
	switch c {
	case sse.Brotli:
		return "brotli"
	case "":
		return "none"
	}
	return c
}

func tierName(t string) string {
	if t == "identity" {
		return "uncompressed"
	}
	return t
}

// byteText formats bytes for reading: "87 B", "9,876 B", "25.4 KB",
// "312 KB", "1.24 MB".
func byteText(n int64) string {
	f := float64(n)
	switch {
	case n < 10_000:
		return thousands(n) + " B"
	case n < 100_000:
		return decimals(f/1e3, 1) + " KB"
	case n < 1_000_000:
		return decimals(f/1e3, 0) + " KB"
	case n < 10_000_000:
		return decimals(f/1e6, 2) + " MB"
	case n < 100_000_000:
		return decimals(f/1e6, 1) + " MB"
	case n < 1_000_000_000:
		return decimals(f/1e6, 0) + " MB"
	}
	return decimals(f/1e9, 2) + " GB"
}

// mibText formats memory: "848 KiB", "1.66 MiB", "96 MiB"; "none" for 0.
func mibText(n int64) string {
	f := float64(n) / (1 << 20)
	switch {
	case n <= 0:
		return "none"
	case n < 1<<20:
		return strconv.FormatInt((n+512)>>10, 10) + " KiB"
	case n < 10<<20:
		return decimals(f, 2) + " MiB"
	case n < 100<<20:
		return decimals(f, 1) + " MiB"
	}
	return decimals(f, 0) + " MiB"
}

// ratioText is events to wire: "69:1", "3.4:1"; "1:1" before anything went
// out.
func ratioText(events, wire int64) string {
	if wire <= 0 || events <= wire {
		return "1:1"
	}
	r := float64(events) / float64(wire)
	if r < 10 {
		return decimals(r, 1) + ":1"
	}
	return thousands(int64(math.Round(r))) + ":1"
}

// decimals formats f with up to d decimals, without trailing zeros.
func decimals(f float64, d int) string {
	s := strconv.FormatFloat(f, 'f', d, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// tiersText says what the open streams hold, the most common first:
// "zstd 2^19 (5), zstd asleep (2)".
func tiersText(m map[string]int) string {
	type tier struct {
		name string
		n    int
	}
	var ts []tier
	for k, n := range m {
		ts = append(ts, tier{tierName(k), n})
	}
	slices.SortFunc(ts, func(a, b tier) int {
		if c := cmp.Compare(b.n, a.n); c != 0 {
			return c
		}
		return cmp.Compare(a.name, b.name)
	})
	parts := make([]string, 0, 4)
	for i, t := range ts {
		if i == 3 {
			parts = append(parts, strconv.Itoa(len(ts)-3)+" more")
			break
		}
		parts = append(parts, t.name+" ("+strconv.Itoa(t.n)+")")
	}
	return strings.Join(parts, ", ")
}
