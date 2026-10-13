package checkboxes

import (
	"net/http"
	"time"

	"github.com/zweiundeins/lith/site/internal/demo"
)

func mount(k *demo.Kit) {
	rt.Store(&deps{hub: k.Hub, w: k.W})

	// A click: the box and the state it should take. Setting a box twice
	// changes it once, so a retried request needs no op id.
	k.Command("set", demo.Rate{Every: setEvery, Burst: setBurst}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Box     int
			Checked bool
		}
		if !demo.Decode(w, r, &req) {
			return
		}
		k.Do(w, r, SetBox{Box: req.Box, Checked: req.Checked, At: time.Now()})
	})

	// A window: what the list asks for as it scrolls (sb-window), and its
	// tiles per row.
	k.Command("window", demo.Rate{Every: windowEvery, Burst: windowBurst}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tab                 string
			Offset, Count, Cols int
		}
		if !demo.Decode(w, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok || req.Offset < 0 || req.Offset >= Chunks {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		win := windowFor(req.Offset, min(max(req.Count, 1), Chunks), req.Cols)
		k.Do(w, r, SetWindow{Viewer: v, Window: win, At: time.Now(), Hub: k.Hub})
	})
}
