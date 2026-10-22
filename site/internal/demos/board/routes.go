package board

import (
	"net/http"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
)

func mount(k *demo.Kit) {
	k.Action("move", demo.Rate{Every: moveEvery, Burst: moveBurst}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tab, Op     string
			Card        int64
			Lane, Index int
			Seen        int64
		}
		if !demo.Decode(w, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok || !demo.ValidID(req.Op) {
			w.WriteHeader(http.StatusBadRequest) // no page of ours sends it
			return
		}
		cmd := Move{Viewer: v, Who: WhoOf(v.Session), Op: req.Op, Card: req.Card, Lane: req.Lane, Index: req.Index, Seen: req.Seen, At: time.Now()}
		// A retried request (a flaky network) moves the card once.
		k.Act(w, r, v, demo.Ack{"ack": req.Op, "ackv": 0, "ok": false}, writer.Once(Slug+":"+v.Session+":"+req.Op, cmd))
	})
	k.Action("hello", demo.Rate{Every: helloEvery, Burst: helloBurst}, func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Tab string }
		if !demo.Decode(w, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok {
			w.WriteHeader(http.StatusBadRequest) // no page of ours sends it
			return
		}
		k.Act(w, r, v, nil, Hello{Viewer: v, Who: WhoOf(v.Session), At: time.Now()})
	})
}
