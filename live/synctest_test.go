package live_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zweiundeins/lith/live"
)

// A stream with Reconnect listens for a better tier after a minute. One
// that can get none (no brotli here) must stay durably blocked, or a
// testing/synctest bubble's clock stops: sse.Stream.Upgrade returned a
// package-level channel, made outside any bubble.
func TestStreamOpenForAMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := live.New(live.Options{})
		ctx, stop := context.WithCancel(t.Context())
		defer stop()
		go h.Run(ctx)
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/sse", nil)
		go h.Serve(httptest.NewRecorder(), r, live.Spec{
			Reconnect: "stream",
			Views:     []live.View{{Key: "v", Render: func(context.Context, *sql.Tx) (any, error) { return []byte("x"), nil }}},
		})
		synctest.Wait()
		time.Sleep(2 * time.Minute)
		if h.Stats().Streams != 1 {
			t.Errorf("%d streams open after two minutes, want 1", h.Stats().Streams)
		}
	})
}
