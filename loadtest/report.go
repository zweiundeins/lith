package loadtest

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// Report is what a run measured. Counts cover the measured time only,
// after the warmup.
type Report struct {
	Streams    int
	Producers  int
	Rate       float64       // commands a second, as configured
	Duration   time.Duration // the measured time, as it ran
	GOMAXPROCS int
	// CPU is the CPU time the process used, user and system, in cores
	// (0 where the platform does not say).
	CPU float64

	// The render loop, from live.Stats and live.Options.OnTick.
	Ticks         int64
	Tick          Percentiles // the duration of each tick
	Overruns      int64
	Renders       int64
	FramesSent    int64
	FramesDropped int64

	// Wire is what went out to the streams, by content coding
	// ("identity" for none).
	Wire map[string]Wire

	// HeapPerStream is the heap that stays in use once the streams have
	// had their frames, beyond what the run held before they opened,
	// divided by Streams.
	HeapPerStream int64

	// The writer, from writer.Stats: the commands it ran and the
	// transactions it ran them in.
	Commands  int64
	Batches   int64
	MaxBatch  int           // the most commands one transaction took in the whole run
	P99Commit time.Duration // of the last 1024 transactions

	// The producers' commands that were due in the measured time.
	Sent    int64
	Errors  int64       // commands whose Do returned an error
	Latency Percentiles // from when a command was due until its commit
}

// Percentiles describe a distribution of durations.
type Percentiles struct {
	N                  int
	P50, P95, P99, Max time.Duration
}

// Wire is what the streams of one content coding received. A write
// carries the frames that reach a stream together: one frame for a
// stream that watches one view.
type Wire struct {
	Streams int
	Writes  int64
	Bytes   int64
}

// PerWrite is the mean size of a write on the wire, in bytes.
func (w Wire) PerWrite() float64 { return ratio(w.Bytes, w.Writes) }

// RendersPerTick is the mean number of views a tick rendered.
func (r Report) RendersPerTick() float64 { return ratio(r.Renders, r.Ticks) }

// CommandsPerSecond is how many commands the writer ran a second.
func (r Report) CommandsPerSecond() float64 { return r.perSecond(r.Commands) }

// CommandsPerBatch is how many commands a transaction took on average.
func (r Report) CommandsPerBatch() float64 { return ratio(r.Commands, r.Batches) }

func (r Report) perSecond(n int64) float64 {
	if r.Duration <= 0 {
		return 0
	}
	return float64(n) / r.Duration.Seconds()
}

func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// percentiles sorts d and picks from it the way the hub's overrun log
// does.
func percentiles(d []time.Duration) Percentiles {
	if len(d) == 0 {
		return Percentiles{}
	}
	slices.Sort(d)
	p := func(q int) time.Duration { return d[q*(len(d)-1)/100] }
	return Percentiles{N: len(d), P50: p(50), P95: p(95), P99: p(99), Max: d[len(d)-1]}
}

func (p Percentiles) String() string {
	r := func(d time.Duration) time.Duration { return d.Round(10 * time.Microsecond) }
	return fmt.Sprintf("p50 %v, p95 %v, p99 %v, max %v", r(p.P50), r(p.P95), r(p.P99), r(p.Max))
}

// String lays the report out in lines, for a log or a benchmark's
// output.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d streams, %d producers at %g commands/s, measured for %v, GOMAXPROCS %d, %.2f cores busy\n",
		r.Streams, r.Producers, r.Rate, r.Duration.Round(time.Millisecond), r.GOMAXPROCS, r.CPU)
	fmt.Fprintf(&b, "ticks: %d, %.2f renders each, %v, %d overruns\n", r.Ticks, r.RendersPerTick(), r.Tick, r.Overruns)
	fmt.Fprintf(&b, "frames: %d sent, %d dropped\n", r.FramesSent, r.FramesDropped)
	for _, c := range slices.Sorted(maps.Keys(r.Wire)) {
		w := r.Wire[c]
		fmt.Fprintf(&b, "wire, %s: %d streams, %d writes, %.0f bytes per write\n", c, w.Streams, w.Writes, w.PerWrite())
	}
	fmt.Fprintf(&b, "heap: %.2f MiB per stream\n", float64(r.HeapPerStream)/(1<<20))
	fmt.Fprintf(&b, "writer: %d commands, %.0f/s, %.2f per transaction, at most %d, p99 commit %v\n",
		r.Commands, r.CommandsPerSecond(), r.CommandsPerBatch(), r.MaxBatch, r.P99Commit.Round(10*time.Microsecond))
	fmt.Fprintf(&b, "commands: %d due, %d errors, due to commit %v", r.Sent, r.Errors, r.Latency)
	return b.String()
}

// Metric is one number of a report, named for testing.B.ReportMetric.
type Metric struct {
	Value float64
	Unit  string
}

// Metrics lists the report's main numbers with units that benchstat can
// compare, one wire size per content coding:
//
//	for _, m := range r.Metrics() {
//		b.ReportMetric(m.Value, m.Unit)
//	}
func (r Report) Metrics() []Metric {
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	m := []Metric{
		{ms(r.Tick.P50), "tick-p50-ms"},
		{ms(r.Tick.P95), "tick-p95-ms"},
		{ms(r.Tick.P99), "tick-p99-ms"},
		{ms(r.Tick.Max), "tick-max-ms"},
		{r.perSecond(r.Ticks), "ticks/s"},
		{r.RendersPerTick(), "renders/tick"},
		{r.perSecond(r.FramesSent), "frames/s"},
		{r.perSecond(r.FramesDropped), "dropped/s"},
	}
	for _, c := range slices.Sorted(maps.Keys(r.Wire)) {
		m = append(m, Metric{r.Wire[c].PerWrite(), c + "-B/write"})
	}
	return append(m,
		Metric{float64(r.HeapPerStream) / (1 << 20), "MiB/stream"},
		Metric{r.CPU, "cores"},
		Metric{r.CommandsPerSecond(), "cmds/s"},
		Metric{r.CommandsPerBatch(), "cmds/tx"},
		Metric{ms(r.Latency.P50), "cmd-p50-ms"},
		Metric{ms(r.Latency.P99), "cmd-p99-ms"},
	)
}
