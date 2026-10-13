package stream

import (
	"net/http"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// A session changes its payload four times a second at most, after eight
// changes at once.
const (
	changeEvery = 250 * time.Millisecond
	changeBurst = 8
)

func mount(k *demo.Kit) {
	k.Command("payload", demo.Rate{Every: changeEvery, Burst: changeBurst}, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tab, Op     string
			Size, Churn string
		}
		if !demo.Decode(w, r, &req) {
			return
		}
		v, ok := demo.Who(r, req.Tab)
		if !ok || !demo.ValidID(req.Op) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		cmd := SetPayload{Viewer: v, Op: req.Op, Size: Size(req.Size), Churn: Churn(req.Churn), At: time.Now()}
		k.Do(w, r, writer.Once(Slug+":"+v.Session+":"+req.Op, cmd))
	})
}
