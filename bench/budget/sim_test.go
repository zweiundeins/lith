package budget

import (
	"encoding/csv"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/zweiundeins/lith/sse"
)

// measured is what one setting cost on one workload, from settings.csv.
type measured struct {
	heap     float64 // MiB per stream
	perFrame float64 // bytes per frame on the wire
}

func readSettings(t *testing.T) map[string]map[string]measured {
	t.Helper()
	f, err := os.Open("settings.csv")
	if err != nil {
		t.Fatalf("%v: run TestSettings first", err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	col := map[string]int{}
	for i, name := range rows[0] {
		col[name] = i
	}
	out := map[string]map[string]measured{}
	for _, r := range rows[1:] {
		w := r[col["workload"]]
		if out[w] == nil {
			out[w] = map[string]measured{}
		}
		heap, _ := strconv.ParseFloat(r[col["heap_mib_per_stream"]], 64)
		per, _ := strconv.ParseFloat(r[col["bytes_per_frame"]], 64)
		out[w][r[col["setting"]]] = measured{heap, per}
	}
	return out
}

// TestSimulation opens streams one by one, up to 2,000, for each workload
// and policy, and after every ten it adds up the heap of their encoders
// and the mean bytes per frame on the wire, from what each setting
// measured (settings.csv). Without a budget every stream has the same
// setting: lith's default, or a static low-memory window at the workload's
// floor. The budgets of 128 MiB are sse.Budget itself, with MinWindow at
// the workload's floor: "budget at open" only opens the streams, "budget"
// also has every open stream write after every ten opens, so that tiers
// shrink and waiting streams take room as it frees up, and "budget, 503"
// turns away the streams it has no room for. A budget of 1 GiB shows the
// tiers over a wider range. It writes simulation.csv.
func TestSimulation(t *testing.T) {
	request(t)
	m := readSettings(t)
	const limit, most, step = 128 << 20, 2000, 10
	f, err := os.Create("simulation.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := csv.NewWriter(f)
	c.Write([]string{"workload", "policy", "streams", "open", "compressed", "uncompressed", "refused", "encoder_heap_mib", "reserved_mib", "bytes_per_frame", "tiers"})
	for _, w := range []struct {
		name   string
		floor  int
		static string
	}{
		{"board", 18, "zstd 2^18 low memory"},
		{"page", 17, "zstd 2^17 low memory"},
	} {
		plain := m[w.name]["identity"].perFrame
		for _, policy := range []string{"lith default", "static low memory", "budget at open", "budget", "budget, 503", "budget of 1 GiB"} {
			var b *sse.Budget
			switch policy {
			case "budget at open", "budget":
				b = sse.NewBudget(sse.BudgetOptions{Limit: limit, MinWindow: w.floor, MaxWindow: 19})
			case "budget, 503":
				b = sse.NewBudget(sse.BudgetOptions{Limit: limit, MinWindow: w.floor, MaxWindow: 19, Refuse: true})
			case "budget of 1 GiB":
				b = sse.NewBudget(sse.BudgetOptions{Limit: 1 << 30, MinWindow: w.floor, MaxWindow: 19})
			}
			var open []*sse.Stream
			refused := 0
			for n := 0; n <= most; n += step {
				tiers := map[string]int{}
				if b == nil {
					s := "zstd 2^19"
					if policy == "static low memory" {
						s = w.static
					}
					if n > 0 {
						tiers[s] = n
					}
				} else {
					for len(open)+refused < n {
						r := httptestRequest()
						s, err := sse.Open(&sink{header: http.Header{}}, r, sse.Options{Budget: b})
						if err != nil {
							refused++
							continue
						}
						open = append(open, s)
					}
					if policy != "budget at open" {
						for _, s := range open {
							if err := s.Write(sse.Comment("x")); err != nil {
								t.Fatal(err)
							}
						}
					}
					tiers = b.Stats().Streams
				}
				var heap, bytes float64
				compressed, uncompressed := 0, 0
				for k, count := range tiers {
					if k == "zstd asleep" || k == "identity" {
						uncompressed += count
						heap += float64(count) * m[w.name]["identity"].heap
						bytes += float64(count) * plain
						continue
					}
					s, ok := m[w.name][k]
					if !ok {
						t.Fatalf("no measurement for %q", k)
					}
					compressed += count
					heap += float64(count) * s.heap
					bytes += float64(count) * s.perFrame
				}
				opened := compressed + uncompressed
				per := 0.0
				if opened > 0 {
					per = bytes / float64(opened)
				}
				reserved := 0.0
				if b != nil {
					reserved = float64(b.Stats().Reserved) / mib
				}
				var desc []string
				for _, k := range slices.Sorted(maps.Keys(tiers)) {
					desc = append(desc, fmt.Sprintf("%s: %d", k, tiers[k]))
				}
				c.Write([]string{w.name, policy, strconv.Itoa(n), strconv.Itoa(opened), strconv.Itoa(compressed), strconv.Itoa(uncompressed), strconv.Itoa(refused),
					strconv.FormatFloat(heap, 'f', 1, 64), strconv.FormatFloat(reserved, 'f', 1, 64), strconv.FormatFloat(per, 'f', 0, 64), strings.Join(desc, "; ")})
				if n == 100 || n == 200 || n == 500 || n == 1000 || n == 1500 || n == 2000 {
					t.Logf("%-5s %-17s %4d streams: %4d compressed, %4d not, %4d refused, %7.1f MiB, %6.0f B/frame  %s",
						w.name, policy, n, compressed, uncompressed, refused, heap, per, strings.Join(desc, "; "))
				}
			}
			for _, s := range open {
				s.Close()
			}
		}
	}
	c.Flush()
}

func httptestRequest() *http.Request {
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Encoding", sse.Zstd)
	return r
}
