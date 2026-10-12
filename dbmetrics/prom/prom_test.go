package prom

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// observer is dbmetrics.Observer, restated: this module does not import lith.
type observer interface {
	Observe(op string, elapsed time.Duration, slow bool)
}

var _ observer = (*Metrics)(nil)

func gather(t *testing.T, c prometheus.Collector) map[string]*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*dto.MetricFamily{}
	for _, mf := range mfs {
		out[mf.GetName()] = mf
	}
	return out
}

func byOp(mf *dto.MetricFamily, op string) *dto.Metric {
	for _, m := range mf.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == "op" && l.GetValue() == op {
				return m
			}
		}
	}
	return nil
}

func TestObservationsBecomeSeries(t *testing.T) {
	m := New("app")
	m.Observe("query", 2*time.Millisecond, false)
	m.Observe("query", 300*time.Millisecond, true)
	m.Observe("exec", time.Millisecond, false)

	mfs := gather(t, m)
	dur := mfs["app_db_query_duration_seconds"]
	if dur == nil {
		t.Fatalf("no duration histogram among %v", mfs)
	}
	if got := byOp(dur, "query").GetHistogram().GetSampleCount(); got != 2 {
		t.Errorf("query samples = %d, want 2", got)
	}
	if got := byOp(dur, "exec").GetHistogram().GetSampleCount(); got != 1 {
		t.Errorf("exec samples = %d, want 1", got)
	}
	slow := mfs["app_db_slow_queries_total"]
	if got := byOp(slow, "query").GetCounter().GetValue(); got != 1 {
		t.Errorf("slow queries = %v, want 1", got)
	}
	if got := byOp(slow, "exec").GetCounter().GetValue(); got != 0 {
		t.Errorf("slow execs = %v, want 0, present before the first one", got)
	}
}

func TestBuckets(t *testing.T) {
	mfs := gather(t, New("app", 0.5, 1))
	h := byOp(mfs["app_db_query_duration_seconds"], "query").GetHistogram()
	if n := len(h.GetBucket()); n != 2 {
		t.Errorf("buckets = %d, want the 2 given", n)
	}
	mfs = gather(t, New("app"))
	h = byOp(mfs["app_db_query_duration_seconds"], "query").GetHistogram()
	if n := len(h.GetBucket()); n != len(DefaultBuckets) {
		t.Errorf("buckets = %d, want the %d defaults", n, len(DefaultBuckets))
	}
}
