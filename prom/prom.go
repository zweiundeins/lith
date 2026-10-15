// Package prom exports a lith app's numbers to Prometheus: the writer's
// batches and commands, the live hub's ticks, renders and frames, and what
// the streams hold of the memory budget. It is a module of its
// own, so that projects without Prometheus never pull it in. Statement
// timings come from dbmetrics/prom.
//
//	reg := prometheus.NewRegistry()
//	reg.MustRegister(prom.New("myapp", prom.Sources{Writer: w, Hub: hub}))
package prom

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"
)

// Sources are what the collector reads; a nil source exports nothing.
type Sources struct {
	Writer *writer.Writer
	Hub    *live.Hub
	// Budget defaults to the hub's (live.Hub.Budget).
	Budget *sse.Budget
}

type collector struct {
	s Sources

	batches, commands, failures, busy, queue, batchMax, commitP99 *prometheus.Desc

	ticks, renders, sent, dropped, deferred, overruns, readTxs, streams, views *prometheus.Desc

	limit, reserved, frames, tier, refused, reopened *prometheus.Desc
}

// New returns a collector of the sources' numbers, named under namespace:
// <namespace>_writer_*, <namespace>_live_* and <namespace>_sse_budget_*.
// It reads the sources' Stats on every scrape.
func New(namespace string, s Sources) prometheus.Collector {
	if s.Budget == nil && s.Hub != nil {
		s.Budget = s.Hub.Budget()
	}
	d := func(sub, name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(prometheus.BuildFQName(namespace, sub, name), help, labels, nil)
	}
	return &collector{
		s:         s,
		batches:   d("writer", "batches_total", "Transactions the writer ran, committed or not."),
		commands:  d("writer", "commands_total", "Commands the writer ran, failed ones included."),
		failures:  d("writer", "command_failures_total", "Commands that returned an error or panicked, or whose batch failed."),
		busy:      d("writer", "busy_seconds_total", "Time the writer spent in batches."),
		queue:     d("writer", "queue", "Commands waiting for the writer."),
		batchMax:  d("writer", "batch_max", "The most commands one batch took."),
		commitP99: d("writer", "commit_p99_seconds", "99th percentile of the last 1024 batches, from BEGIN to the end of COMMIT."),
		ticks:     d("live", "ticks_total", "Ticks that rendered views."),
		renders:   d("live", "renders_total", "Views rendered."),
		sent:      d("live", "frames_sent_total", "Frames of views written to streams."),
		dropped:   d("live", "frames_dropped_total", "Frames a newer one replaced before their stream sent them."),
		deferred:  d("live", "renders_deferred_total", "Renders of dirty views put off while every stream watching the view had a frame of it waiting."),
		overruns:  d("live", "tick_overruns_total", "Ticks that took longer than the tick."),
		readTxs:   d("live", "read_txs_total", "Read transactions the render workers began."),
		streams:   d("live", "streams", "Streams open now."),
		views:     d("live", "views", "Views watched now."),
		limit:     d("sse_budget", "limit_bytes", "The memory the streams may hold together, their encoders and the frames kept for them, 0 for none."),
		reserved:  d("sse_budget", "reserved_bytes", "The memory the open streams hold: their encoders, 32 KiB each besides, and the frames kept for them."),
		frames:    d("sse_budget", "frames_bytes", "The memory the frames kept for the streams hold (the newest of the hub's views that two or more streams watch)."),
		tier:      d("sse_budget", "streams", "Open streams by what their encoder holds.", "setting"),
		refused:   d("sse_budget", "refused_total", "Streams the budget turned away."),
		reopened:  d("sse_budget", "reopened_total", "Streams the budget asked to open again for a better tier."),
	}
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.batches, c.commands, c.failures, c.busy, c.queue, c.batchMax, c.commitP99,
		c.ticks, c.renders, c.sent, c.dropped, c.deferred, c.overruns, c.readTxs, c.streams, c.views,
		c.limit, c.reserved, c.frames, c.tier, c.refused, c.reopened} {
		ch <- d
	}
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	counter := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, labels...)
	}
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	if w := c.s.Writer; w != nil {
		st := w.Stats()
		counter(c.batches, float64(st.Batches))
		counter(c.commands, float64(st.Commands))
		counter(c.failures, float64(st.Failures))
		counter(c.busy, st.Busy.Seconds())
		gauge(c.queue, float64(st.Queue))
		gauge(c.batchMax, float64(st.MaxBatch))
		gauge(c.commitP99, st.P99Commit.Seconds())
	}
	if h := c.s.Hub; h != nil {
		st := h.Stats()
		counter(c.ticks, float64(st.Ticks))
		counter(c.renders, float64(st.Renders))
		counter(c.sent, float64(st.FramesSent))
		counter(c.dropped, float64(st.FramesDropped))
		counter(c.deferred, float64(st.RendersDeferred))
		counter(c.overruns, float64(st.Overruns))
		counter(c.readTxs, float64(st.ReadTxs))
		gauge(c.streams, float64(st.Streams))
		gauge(c.views, float64(st.Views))
	}
	if b := c.s.Budget; b != nil {
		st := b.Stats()
		gauge(c.limit, float64(st.Limit))
		gauge(c.reserved, float64(st.Reserved))
		gauge(c.frames, float64(st.Frames))
		for setting, n := range st.Streams {
			gauge(c.tier, float64(n), setting)
		}
		counter(c.refused, float64(st.Refused))
		counter(c.reopened, float64(st.Reopened))
	}
}
