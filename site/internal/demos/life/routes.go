package life

import (
	"net/http"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// The actions' rates per session; an address gets four times as much. A
// stroke sends ten requests a second while it lasts: it stays out of the
// site's limit for all actions, which it would use up.
var (
	drawRate   = demo.Rate{Every: 100 * time.Millisecond, Burst: 20, Separate: true}
	dropRate   = demo.Rate{Every: 500 * time.Millisecond, Burst: 6}
	resetRate  = demo.Rate{Every: ResetEvery, Burst: 2}
	colourRate = demo.Rate{Every: time.Second, Burst: 5}
)

func mount(k *demo.Kit) {
	if k.Hub != nil {
		c := &clock{w: k.W, hub: k.Hub, log: k.Log}
		go c.run()
	}
	k.Action("draw", drawRate, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tab, Op string
			Cells   []int
			Alive   bool
		}
		v, ok := who(w, r, &req, &req.Tab, &req.Op)
		if !ok {
			return
		}
		// A stroke sent twice draws the same cells: no Once needed.
		k.Act(w, r, v, demo.Ack{"ack": req.Op}, Draw{Viewer: v, Op: req.Op, Cells: req.Cells, Alive: req.Alive, At: time.Now()})
	})
	k.Action("drop", dropRate, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tab, Op, Pattern string
			Cell, Turn       int
		}
		v, ok := who(w, r, &req, &req.Tab, &req.Op)
		if !ok {
			return
		}
		cmd := Drop{Viewer: v, Op: req.Op, Pattern: req.Pattern, Cell: req.Cell, Turn: req.Turn, At: time.Now()}
		k.Act(w, r, v, demo.Ack{"ack": req.Op}, writer.Once(Slug+":"+v.Session+":"+req.Op, cmd))
	})
	k.Action("reset", resetRate, func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Tab, Op string }
		v, ok := who(w, r, &req, &req.Tab, &req.Op)
		if !ok {
			return
		}
		k.Act(w, r, v, demo.Ack{"ack": req.Op}, writer.Once(Slug+":"+v.Session+":"+req.Op, Reset{Viewer: v, Op: req.Op, At: time.Now()}))
	})
	k.Action("colour", colourRate, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tab    string
			Colour int
		}
		if !demo.Decode(w, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok {
			w.WriteHeader(http.StatusBadRequest) // no page of ours sends it
			return
		}
		// A notice sets the swatch back to the colour the session has.
		was, err := colourOf(r.Context(), k.DB.R, v.Session)
		if err != nil {
			k.Fail(w, r, err)
			return
		}
		k.Act(w, r, v, demo.Ack{"pick": was}, Pick{Viewer: v, Colour: req.Colour, At: time.Now()})
	})
}

// who decodes an action's payload into req and returns its viewer, from
// the tab and the op the payload carries. When either is not an id a page
// makes, it answers a bare 400 and reports false.
func who(w http.ResponseWriter, r *http.Request, req any, tab, op *string) (demo.Viewer, bool) {
	if !demo.Decode(w, r, req) {
		return demo.Viewer{}, false
	}
	v, ok := demo.Who(r, *tab)
	if !ok || !demo.ValidID(*op) {
		w.WriteHeader(http.StatusBadRequest)
		return demo.Viewer{}, false
	}
	return v, true
}
