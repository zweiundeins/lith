// Package metrics counts the site's HTTP requests for Prometheus: by route
// pattern and status code, with their durations. A route is the ServeMux
// pattern that matched ("GET /demos/{slug}"), never the raw path, so the
// number of series stays small whatever URLs arrive.
package metrics

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// HTTP is a prometheus.Collector of <namespace>_http_requests_total{route,
// code}, <namespace>_http_request_duration_seconds{route} and
// <namespace>_http_requests_in_flight.
type HTTP struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
}

// NewHTTP returns the collector, named under namespace.
func NewHTTP(namespace string) *HTTP {
	return &HTTP{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "http_requests_total",
			Help: "HTTP requests by route (the ServeMux pattern; edge for those the edge answered) and status code.",
		}, []string{"route", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "http_request_duration_seconds",
			Help:    "Time to answer an HTTP request, by route. Event streams are left out: they last as long as their tab.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"route"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "http_requests_in_flight",
			Help: "HTTP requests being answered now, open event streams included.",
		}),
	}
}

func (m *HTTP) Describe(ch chan<- *prometheus.Desc) {
	m.requests.Describe(ch)
	m.duration.Describe(ch)
	m.inFlight.Describe(ch)
}

func (m *HTTP) Collect(ch chan<- prometheus.Metric) {
	m.requests.Collect(ch)
	m.duration.Collect(ch)
	m.inFlight.Collect(ch)
}

type routeKey struct{}

// Measure counts and times every request through next. It goes around the
// whole chain, so it sees what the edge refuses too; Route, around the
// mux, tells it the pattern that matched.
func (m *HTTP) Measure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		route := new(string)
		rec := &recorder{ResponseWriter: w}
		m.inFlight.Inc()
		defer m.inFlight.Dec()
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), routeKey{}, route)))
		label := *route
		if label == "" {
			label = "edge"
		}
		code := rec.code
		if code == 0 {
			code = http.StatusOK
		}
		m.requests.WithLabelValues(label, strconv.Itoa(code)).Inc()
		if !rec.stream {
			m.duration.WithLabelValues(label).Observe(time.Since(start).Seconds())
		}
	})
}

// Route goes around the mux: once the mux has served a request, it hands
// Measure the pattern that matched ("/" for the catch-all).
func Route(mux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		if route, ok := r.Context().Value(routeKey{}).(*string); ok {
			*route = r.Pattern
		}
	})
}

// recorder notes the status code and whether the answer is an event
// stream. It unwraps for http.ResponseController, so streams still set
// their write deadlines and flush.
type recorder struct {
	http.ResponseWriter
	code   int
	stream bool
}

func (r *recorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
		r.stream = strings.HasPrefix(r.Header().Get("Content-Type"), "text/event-stream")
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Flush() {
	if r.code == 0 {
		r.WriteHeader(http.StatusOK)
	}
	http.NewResponseController(r.ResponseWriter).Flush()
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
