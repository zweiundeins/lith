package canvas

import (
	"net/http"
	"slices"
	"time"

	"github.com/zweiundeins/lith/edge"

	"github.com/zweiundeins/lith/site/internal/demo"
)

func mount(k *demo.Kit) {
	// Pixels, on top of the kit's limit on requests: a token bucket per
	// session, and one with four times as much per address. The address
	// comes first, so a client that makes up sessions makes no buckets.
	bySession := edge.NewLimiter(pixelRate, pixelBurst)
	byAddr := edge.NewLimiter(4*pixelRate, 4*pixelBurst)
	k.Command("paint", demo.Rate{Every: paintEvery, Burst: paintBurst, Separate: true}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tab   string
			Color int
			Cells []int
		}
		if !demo.Decode(w, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		slices.Sort(req.Cells)
		now := time.Now()
		cmd := Paint{Viewer: v, Color: req.Color, Cells: slices.Compact(req.Cells), At: now}
		if err := cmd.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if n := len(cmd.Cells); !byAddr.Allow(edge.ClientKey(r), n, now) || !bySession.Allow(v.Session, n, now) {
			http.Error(w, "Painting too fast: 20 pixels a second each.", http.StatusTooManyRequests)
			return
		}
		k.Do(w, r, cmd)
	})
	k.Command("pick", demo.Rate{Every: pickEvery, Burst: pickBurst}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tab   string
			Color int
		}
		if !demo.Decode(w, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		k.Do(w, r, Pick{Viewer: v, Color: req.Color, At: time.Now()})
	})
}
