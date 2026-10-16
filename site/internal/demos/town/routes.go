package town

import (
	"net/http"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
)

func mount(k *demo.Kit) {
	// The starting town, laid out once, when the writer starts.
	if err := k.W.Send(Seed(time.Now())); err != nil {
		k.Log.Warn("town: seed", "err", err)
	}
	k.Command("place", demo.Rate{Every: placeEvery, Burst: placeBurst}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tab, Op string
			Tile    int
			Kind    string
			Seen    int64
		}
		if !demo.Decode(w, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok || !demo.ValidID(req.Op) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		cmd := Place{Viewer: v, Who: whoOf(v.Session), Op: req.Op, Tile: req.Tile, Kind: req.Kind, Seen: req.Seen, At: time.Now()}
		// A retried request (a flaky network) changes the tile once.
		k.Do(w, r, writer.Once(Slug+":"+v.Session+":"+req.Op, cmd))
	})
	// The slider: a position each tick while it is dragged. Separate keeps
	// a drag out of the site's limit for all actions, which it would use
	// up for every other demo.
	k.Command("seek", demo.Rate{Every: seekEvery, Burst: seekBurst, Separate: true}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tab     string
			At, Seq int64
		}
		if !demo.Decode(w, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		k.Do(w, r, Seek{Viewer: v, At: req.At, Seq: req.Seq, Now: time.Now(), Hub: k.Hub})
	})
	k.Command("follow", demo.Rate{Every: followEvery, Burst: followBurst}, func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Tab string }
		if !demo.Decode(w, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		k.Do(w, r, Follow{Viewer: v, Hub: k.Hub})
	})
}
