// Package prom exports a lith app's numbers to Prometheus: the writer's
// batches and commands, the live hub's ticks, renders and frames, what
// the streams hold of the memory budget, and the event store's appends
// and projectors. It is a module of its own, so that projects without
// Prometheus never pull it in. Statement timings come from dbmetrics/prom.
//
//	reg := prometheus.NewRegistry()
//	reg.MustRegister(prom.New("myapp", prom.Sources{Writer: w, Hub: hub}))
package prom

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/zweiundeins/lith/events"
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
	// Events is the runner of the app's projectors in its database, and
	// Store the event store, whose counters cover every database it
	// serves.
	Events *events.Runner
	Store  *events.Store
}

// statuses are the statuses of a projector, a series each.
var statuses = []string{"live", "rebuilding", "parked"}

type collector struct {
	s Sources

	batches, commands, failures, busy, queue, batchMax, commitP99, running, published, undelivered, subscribers *prometheus.Desc

	ticks, renders, sent, dropped, deferred, overruns, readTxs, streams, views, behind, behindStreams, acks, paces, releases, refreshes, unchanged, skipped *prometheus.Desc

	limit, reserved, frames, tier, refused, reopened *prometheus.Desc

	appended, conflicts, refolds, head, position, lag, status, attempts *prometheus.Desc
}

// New returns a collector of the sources' numbers, named under namespace:
// <namespace>_writer_*, <namespace>_live_*, <namespace>_sse_budget_* and
// <namespace>_events_*.
// It reads the sources' Stats on every scrape.
func New(namespace string, s Sources) prometheus.Collector {
	if s.Budget == nil && s.Hub != nil {
		s.Budget = s.Hub.Budget()
	}
	d := func(sub, name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(prometheus.BuildFQName(namespace, sub, name), help, labels, nil)
	}
	return &collector{
		s:             s,
		batches:       d("writer", "batches_total", "Transactions the writer ran, committed or not."),
		commands:      d("writer", "commands_total", "Commands the writer ran, failed ones included."),
		failures:      d("writer", "command_failures_total", "Commands that returned an error or panicked, or whose batch failed."),
		busy:          d("writer", "busy_seconds_total", "Time the writer spent in batches."),
		queue:         d("writer", "queue", "Commands waiting for the writer."),
		batchMax:      d("writer", "batch_max", "The most commands one batch took."),
		commitP99:     d("writer", "commit_p99_seconds", "99th percentile of the last 1024 batches, from BEGIN to the end of COMMIT."),
		running:       d("writer", "command_running_seconds", "How long the command under way has run, 0 between commands: every other command waits for it."),
		published:     d("writer", "published_total", "Values committed commands published to the writer's subscribers (Tx.Publish)."),
		undelivered:   d("writer", "publish_dropped_total", "Published values a subscriber's full queue did not take, once per subscriber."),
		subscribers:   d("writer", "subscribers", "Subscriptions to the writer's published values open now (Writer.Subscribe)."),
		ticks:         d("live", "ticks_total", "Ticks that rendered views."),
		renders:       d("live", "renders_total", "Views rendered."),
		sent:          d("live", "frames_sent_total", "Frames of views written to streams."),
		dropped:       d("live", "frames_dropped_total", "Frames a newer one replaced before their stream sent them."),
		deferred:      d("live", "renders_deferred_total", "Renders of dirty views put off while every stream watching the view had a frame of it waiting."),
		overruns:      d("live", "tick_overruns_total", "Ticks that took longer than the tick."),
		readTxs:       d("live", "read_txs_total", "Read transactions the render workers began."),
		streams:       d("live", "streams", "Streams open now."),
		views:         d("live", "views", "Views watched now."),
		behind:        d("live", "newest_frame_behind_total", "Reports from pages that fell behind their stream (newest-frame-behind, Hub.Behind)."),
		behindStreams: d("live", "newest_frame_behind_streams_total", "Streams whose page reported at least once that it fell behind."),
		acks:          d("live", "acks_total", "Acknowledgements of paced streams' writes (Hub.Ack)."),
		paces:         d("live", "paces_total", "Streams that Hub.Pace started to pace."),
		releases:      d("live", "releases_total", "Paced streams whose page kept up for Spec.Release, and that send every frame again."),
		refreshes:     d("live", "refreshes_total", "Streams whose page a former build rendered and that sent Spec.Refresh's events first."),
		unchanged:     d("live", "refreshes_unchanged_total", "Streams whose page a former build rendered and whose Spec.Refresh found it current."),
		skipped:       d("live", "refreshes_skipped_total", "Refreshes skipped, by reason: error, timeout, busy (no slot within live.RefreshWait) or stream_element (the events held the stream's element).", "reason"),
		limit:         d("sse_budget", "limit_bytes", "The memory the streams may hold together, their encoders and the frames kept for them, 0 for none."),
		reserved:      d("sse_budget", "reserved_bytes", "The memory the open streams hold: their encoders, 32 KiB each besides, and the frames kept for them."),
		frames:        d("sse_budget", "frames_bytes", "The memory the frames kept for the streams hold (the newest of the hub's views that two or more streams watch)."),
		tier:          d("sse_budget", "streams", "Open streams by what their encoder holds.", "setting"),
		refused:       d("sse_budget", "refused_total", "Streams the budget turned away."),
		reopened:      d("sse_budget", "reopened_total", "Streams the budget asked to open again for a better tier."),
		appended:      d("events", "appended_total", "Events committed, by stream kind.", "stream"),
		conflicts:     d("events", "conflicts_total", "Appends refused because the stream was not at the expected version."),
		refolds:       d("events", "refolds_total", "Streams whose state a command folded again from their events, as the state row was missing or from another decider version."),
		head:          d("events", "head_position", "The last position committed in the database."),
		position:      d("events", "projector_position", "A catch-up projector's checkpoint, or how far a rebuild got.", "projector"),
		lag:           d("events", "projector_lag", "Positions between a catch-up projector or a rebuild and the last event of its stream kind; 0 for a live inline projector.", "projector"),
		status:        d("events", "projector_status", "1 for the projector's status (live, rebuilding or parked), 0 for the others.", "projector", "status"),
		attempts:      d("events", "projector_attempts", "Failures of the event a catch-up projector retries.", "projector"),
	}
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.batches, c.commands, c.failures, c.busy, c.queue, c.batchMax, c.commitP99, c.running, c.published, c.undelivered, c.subscribers,
		c.ticks, c.renders, c.sent, c.dropped, c.deferred, c.overruns, c.readTxs, c.streams, c.views, c.behind, c.behindStreams, c.acks, c.paces, c.releases, c.refreshes, c.unchanged, c.skipped,
		c.limit, c.reserved, c.frames, c.tier, c.refused, c.reopened,
		c.appended, c.conflicts, c.refolds, c.head, c.position, c.lag, c.status, c.attempts} {
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
		gauge(c.running, st.Running.Seconds())
		counter(c.published, float64(st.Published))
		counter(c.undelivered, float64(st.Dropped))
		gauge(c.subscribers, float64(st.Subscribers))
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
		counter(c.behind, float64(st.Behind))
		counter(c.behindStreams, float64(st.BehindStreams))
		counter(c.acks, float64(st.Acks))
		counter(c.paces, float64(st.Paces))
		counter(c.releases, float64(st.Releases))
		counter(c.refreshes, float64(st.Refreshes))
		counter(c.unchanged, float64(st.RefreshesUnchanged))
		counter(c.skipped, float64(st.RefreshErrors), "error")
		counter(c.skipped, float64(st.RefreshTimeouts), "timeout")
		counter(c.skipped, float64(st.RefreshesBusy), "busy")
		counter(c.skipped, float64(st.RefreshesGuarded), "stream_element")
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
	if s := c.s.Store; s != nil {
		st := s.Stats()
		for kind, n := range st.Appended {
			counter(c.appended, float64(n), kind)
		}
		counter(c.conflicts, float64(st.Conflicts))
		counter(c.refolds, float64(st.Refolds))
	}
	if r := c.s.Events; r != nil {
		st := r.Stats()
		gauge(c.head, float64(st.Head))
		for _, p := range st.Projectors {
			gauge(c.position, float64(p.Position), p.Name)
			gauge(c.lag, float64(p.Lag), p.Name)
			for _, s := range statuses {
				v := 0.0
				if s == p.Status {
					v = 1
				}
				gauge(c.status, v, p.Name, s)
			}
			gauge(c.attempts, float64(p.Attempts), p.Name)
		}
	}
}
