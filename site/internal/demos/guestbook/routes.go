package guestbook

import (
	"net/http"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
)

func mount(k *demo.Kit) {
	k.Action("sign", demo.Rate{Every: signEvery, Burst: signBurst}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tab, Op       string
			Name, Message string
		}
		if !demo.Decode(w, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok || !demo.ValidID(req.Op) {
			w.WriteHeader(http.StatusBadRequest) // no page of ours sends it
			return
		}
		cmd := Sign{Viewer: v, Op: req.Op, Name: req.Name, Message: req.Message, At: time.Now()}
		// A retried request (a flaky network) signs once.
		k.Act(w, r, v, demo.Ack{"ack": req.Op, "accepted": false}, writer.Once(Slug+":"+v.Session+":"+req.Op, cmd))
	})
}
