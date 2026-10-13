package board

import (
	"net/http"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
)

func mount(k *demo.Kit) {
	k.Command("move", demo.Rate{Every: moveEvery, Burst: moveBurst}, func(w http.ResponseWriter, r *http.Request) {
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
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		cmd := Move{Viewer: v, Who: WhoOf(v.Session), Op: req.Op, Card: req.Card, Lane: req.Lane, Index: req.Index, Seen: req.Seen, At: time.Now()}
		// A retried request (a flaky network) moves the card once.
		k.Do(w, r, writer.Once(Slug+":"+v.Session+":"+req.Op, cmd))
	})
	k.Command("hello", demo.Rate{Every: helloEvery, Burst: helloBurst}, func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Tab string }
		if !demo.Decode(w, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		k.Do(w, r, Hello{Viewer: v, Who: WhoOf(v.Session), At: time.Now()})
	})
}
