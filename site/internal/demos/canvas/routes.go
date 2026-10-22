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
	k.Action("paint", demo.Rate{Every: paintEvery, Burst: paintBurst, Separate: true}, func(w http.ResponseWriter, r *http.Request) {
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
			w.WriteHeader(http.StatusBadRequest) // no page of ours sends it
			return
		}
		slices.Sort(req.Cells)
		now := time.Now()
		cmd := Paint{Viewer: v, Color: req.Color, Cells: slices.Compact(req.Cells), At: now}
		if err := cmd.Validate(); err != nil {
			w.WriteHeader(http.StatusBadRequest) // the page sends only strokes that pass
			return
		}
		// The pixel rate is the canvas's rule, and painting reaches it: a
		// batch over it is refused in the tab's row, which says why.
		n := len(cmd.Cells)
		cmd.Over = !byAddr.Allow(edge.ClientKey(r), n, now) || !bySession.Allow(v.Session, n, now)
		k.Act(w, r, v, nil, cmd)
	})
	k.Action("pick", demo.Rate{Every: pickEvery, Burst: pickBurst}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tab   string
			Color int
		}
		if !demo.Decode(w, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok {
			w.WriteHeader(http.StatusBadRequest) // no page of ours sends it
			return
		}
		k.Act(w, r, v, nil, Pick{Viewer: v, Color: req.Color, At: time.Now()})
	})
}
