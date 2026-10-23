package edge

import (
	"net/http"
	"time"
)

// BodyTimeout is the time BodyDeadline gives a request's body by default,
// as sse.ReadSignals gives an action's.
const BodyTimeout = 10 * time.Second

// BodyDeadline gives the body of every request that has one d to arrive
// (BodyTimeout when d is 0 or less): it sets a read deadline on the
// connection through http.ResponseController before next runs. A handler
// that never reads the body, such as one that answers 204 or renders a
// page, would otherwise hold its connection and a goroutine for as long as
// a client trickles a body it declared: Go's server reads the rest of the
// body before it sends the answer, without a deadline. With one, it gives
// up once the deadline has passed, sends the answer and closes the
// connection.
//
// A request without a body, such as a stream's GET, gets no deadline. Go's
// server clears the deadline itself once the body is in, as it starts to
// watch the connection for a client that leaves, so a handler that read
// the body, or a stream whose headers went out after it, runs on as long
// as it likes. A handler that takes a large body, such as an upload, sets
// a later deadline of its own before it reads:
//
//	http.NewResponseController(w).SetReadDeadline(time.Now().Add(time.Minute))
//
// Mount it outermost, around the probes and the static files too: a
// trickled body holds a connection on any route.
func BodyDeadline(d time.Duration, next http.Handler) http.Handler {
	if d <= 0 {
		d = BodyTimeout
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			// A writer without a connection, such as httptest's recorder,
			// takes no deadline and needs none.
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(d))
		}
		next.ServeHTTP(w, r)
	})
}
