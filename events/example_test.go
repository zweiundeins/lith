package events_test

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/zweiundeins/lith/events"
)

// A catch-up projector posts every new card to another service after the
// commit. The service can see a card twice, after a failure or a restart;
// the event's position, sent as the idempotency key, tells it so.
func ExampleAfter() {
	hook := events.NewProjector(Cards, "card_hook", 1,
		events.After(Created, func(ctx context.Context, r events.Record[int64, CardCreated]) error {
			body := strings.NewReader(fmt.Sprintf(`{"card": %d, "title": %q}`, r.ID, r.Data.Title))
			req, err := http.NewRequestWithContext(ctx, "POST", "https://hooks.example.com/cards", body)
			if err != nil {
				return err
			}
			req.Header.Set("Idempotency-Key", strconv.FormatInt(r.Position, 10))
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				return err // retried with backoff, then parked
			}
			res.Body.Close()
			if res.StatusCode >= 300 {
				return fmt.Errorf("the hook answered %s", res.Status)
			}
			return nil
		}),
		events.Ignore(Renamed),
		events.Ignore(Archived),
	).SkipHistory() // a new hook posts the cards made from now on
	store := events.Must(events.New(hook))
	_ = store // events.NewRunner(store, db, w, events.Options{}).Run(ctx) runs it
}
