package web

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"
)

// HubOptions are the board's hub settings. The budget gives each stream
// the window its largest frame needs: a full board of 150 cards with long
// titles is about 51 KB, which 2^16 holds. A stream that cannot have even
// the smallest tier gets a 503, and the page's retry 'error' asks again,
// instead of a stream that sends every frame uncompressed. When the hub
// stops, a tab's stream ends with a notice in the outcome's place.
func HubOptions(db *sqlite.DB, log *slog.Logger) live.Options {
	return live.Options{
		Read:            db.R,
		MaxStreams:      500,
		Stopping:        stopping,
		HeartbeatSignal: "_ping",     // the page's watchdog reads it (expr.go)
		Hibernate:       time.Minute, // an idle tab gives its zstd encoder back
		Log:             log,
		SSE:             sse.Options{Budget: sse.NewBudget(sse.BudgetOptions{Refuse: true})},
	}
}

// statsView shows the writer's and the hub's numbers on the status page.
// Nothing touches a topic for them, so Every renders it again each second.
func statsView(w *writer.Writer, hub *live.Hub) live.View {
	return live.View{
		Key:   "stats",
		Every: time.Second,
		Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
			return patch(ctx, Stats(w.Stats(), hub.Stats(), hub.Budget().Stats()))
		},
	}
}
