package checkboxes

import (
	"net/http"
	"time"

	"github.com/zweiundeins/lith/site/internal/demo"
)

func mount(k *demo.Kit) {
	// A click: the box and the state it should take, from a tab, with an
	// op id that the tab's view acks (Clicks). Setting a box twice changes
	// it once, so a retried request needs no Once.
	k.Action("set", demo.Rate{Every: setEvery, Burst: setBurst}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tab, Op string
			Box     int
			Checked bool
		}
		if !demo.Decode(w, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok || !demo.ValidID(req.Op) {
			w.WriteHeader(http.StatusBadRequest) // no page of ours sends it
			return
		}
		k.Act(w, r, v, demo.Ack{"ack": req.Op}, SetBox{Viewer: v, Op: req.Op, Box: req.Box, Checked: req.Checked, At: time.Now()})
	})

	// A window: what the list asks for as it scrolls (sb-window), and its
	// tiles per row.
	k.Action("window", demo.Rate{Every: windowEvery, Burst: windowBurst}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tab                 string
			Offset, Count, Cols int
		}
		if !demo.Decode(w, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok || req.Offset < 0 || req.Offset >= Chunks {
			w.WriteHeader(http.StatusBadRequest) // no page of ours sends it
			return
		}
		win := windowFor(req.Offset, min(max(req.Count, 1), Chunks), req.Cols)
		k.Act(w, r, v, nil, SetWindow{Viewer: v, Window: win, At: time.Now(), Hub: k.Hub})
	})
}
