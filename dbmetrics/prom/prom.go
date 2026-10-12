// Package prom records dbmetrics observations in Prometheus. It is a module
// of its own so that projects without Prometheus never pull it in.
//
//	m := prom.New("myapp")
//	prometheus.MustRegister(m)
//	db := sql.OpenDB(dbmetrics.WrapConnector(c, dbmetrics.Options{Observer: m}))
package prom

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// DefaultBuckets span SQLite's range, from 50µs to 5s.
var DefaultBuckets = []float64{.00005, .0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}

// Metrics is a dbmetrics.Observer and a prometheus.Collector of
// <namespace>_db_query_duration_seconds{op} and
// <namespace>_db_slow_queries_total{op}.
type Metrics struct {
	duration *prometheus.HistogramVec
	slow     *prometheus.CounterVec
}

// New returns Metrics named under namespace, with histogram buckets in
// seconds (DefaultBuckets if none are given).
func New(namespace string, buckets ...float64) *Metrics {
	if len(buckets) == 0 {
		buckets = DefaultBuckets
	}
	m := &Metrics{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: "db",
			Name:      "query_duration_seconds",
			Help:      "Time statements spent in the database driver, by op: query (returns rows) or exec.",
			Buckets:   buckets,
		}, []string{"op"}),
		slow: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "db",
			Name:      "slow_queries_total",
			Help:      "Statements at or above the slow threshold, by op. Each is also logged with its SQL text.",
		}, []string{"op"}),
	}
	for _, op := range []string{"query", "exec"} { // dbmetrics.OpQuery, OpExec
		m.duration.WithLabelValues(op)
		m.slow.WithLabelValues(op)
	}
	return m
}

// Observe implements dbmetrics.Observer.
func (m *Metrics) Observe(op string, elapsed time.Duration, slow bool) {
	m.duration.WithLabelValues(op).Observe(elapsed.Seconds())
	if slow {
		m.slow.WithLabelValues(op).Inc()
	}
}

// Describe implements prometheus.Collector.
func (m *Metrics) Describe(ch chan<- *prometheus.Desc) {
	m.duration.Describe(ch)
	m.slow.Describe(ch)
}

// Collect implements prometheus.Collector.
func (m *Metrics) Collect(ch chan<- prometheus.Metric) {
	m.duration.Collect(ch)
	m.slow.Collect(ch)
}
