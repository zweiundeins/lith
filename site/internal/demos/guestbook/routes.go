package guestbook

import (
	"net/http"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
)

func mount(k *demo.Kit) {
	k.Command("sign", demo.Rate{Every: signEvery, Burst: signBurst}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tab, Op       string
			Name, Message string
		}
		if !demo.Decode(w, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok || !demo.ValidID(req.Op) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		cmd := Sign{Viewer: v, Op: req.Op, Name: req.Name, Message: req.Message, At: time.Now()}
		// A retried request (a flaky network) signs once.
		k.Do(w, r, writer.Once(Slug+":"+v.Session+":"+req.Op, cmd))
	})
}
