package demo

import (
	"time"

	"github.com/zweiundeins/lith/sse"
)

// Probe is a frame that reports on the stream that carries it, for a demo
// that shows a tab its own stream. The site's frame writer keeps the newest
// Probe of each view a stream watches and, from then on, calls Report at
// every tick that brings the stream frames, the probe's own included.
// Report reads the stream itself (its coding, its tier, the bytes it sent)
// and the stream's Meter, and returns an event for the same write as the
// tick's frames, or nil. frames counts the tick's other events: with none,
// a report goes out alone, and nil sends nothing.
type Probe interface {
	Report(st *sse.Stream, m *Meter, frames int) []byte
}

// Meter counts the writes of one stream as the site's frame writer makes
// them: one write per tick, the tick's frames compressed and flushed
// together.
type Meter struct {
	// Writes counts the writes; Frames those of them that carried frames
	// of views, and not only reports.
	Writes, Frames int64
	// Last is the newest write that carried frames.
	Last Write
	// Beats counts the hub's heartbeats: the comments it sends a stream
	// that has been silent for a while, found as the bytes of events the
	// stream sent between two writes.
	Beats int64
	// Reported is Frames when a probe last reported, and ReportedAt when:
	// a probe keeps them, to tell what its reports have not described yet.
	Reported   int64
	ReportedAt time.Time

	first time.Time
	times [64]int64 // when the newest frame writes went out, unix nanoseconds, a ring
}

// Write is one write to a stream: the bytes of its events, the bytes that
// went out for them, and when.
type Write struct {
	Events, Wire int64
	At           time.Time
}

// Rate is the frames a second the stream sent over window, up to now, or
// since its first frame if that is more recent.
func (m *Meter) Rate(now time.Time, window time.Duration) float64 {
	n := min(m.Frames, int64(len(m.times)))
	since := now.Add(-window)
	if m.first.After(since) {
		since = m.first
	}
	from := since.UnixNano()
	k := int64(0)
	for ; k < n; k++ {
		if m.times[(m.Frames-1-k)%int64(len(m.times))] < from {
			break
		}
	}
	span := now.Sub(since)
	if k == int64(len(m.times)) {
		// The ring covers less than the window: the rate over what it holds.
		span = now.Sub(time.Unix(0, m.times[m.Frames%int64(len(m.times))]))
	}
	if span < time.Second {
		span = time.Second
	}
	return float64(k) / span.Seconds()
}

// heartbeat is the bytes of events of one of the hub's heartbeats: an SSE
// comment (live.Options.HeartbeatSignal is not set on the site).
var heartbeat = int64(len(sse.Comment("")))

// Probes are the probes one stream carries, and its Meter. The site's frame
// writer keeps one per stream and writes every tick through it.
type Probes struct {
	list   []keyed
	m      Meter
	events int64    // the stream's bytes of events after its newest write
	buf    [][]byte // a tick's events and reports, reused
}

type keyed struct {
	key string
	p   Probe
}

// Keep makes p the probe of the view with the key.
func (ps *Probes) Keep(key string, p Probe) {
	for i := range ps.list {
		if ps.list[i].key == key {
			ps.list[i].p = p
			return
		}
	}
	ps.list = append(ps.list, keyed{key, p})
}

// Write writes a tick's events to the stream in one write, with the reports
// of the probes, and measures it. It writes nothing when there is nothing
// to write.
func (ps *Probes) Write(st *sse.Stream, events [][]byte) error {
	frames := len(events)
	if len(ps.list) > 0 {
		ev, _ := st.Sent()
		ps.m.Beats += (ev - ps.events) / heartbeat
		ps.events = ev
		ps.buf = append(ps.buf[:0], events...)
		for _, k := range ps.list {
			if e := k.p.Report(st, &ps.m, frames); e != nil {
				ps.buf = append(ps.buf, e)
			}
		}
		events = ps.buf
		defer clear(ps.buf)
	}
	if len(events) == 0 {
		return nil
	}
	ev, wire := st.Sent()
	err := st.Write(events...)
	ev2, wire2 := st.Sent()
	ps.events = ev2
	m := &ps.m
	m.Writes++
	if frames > 0 {
		now := time.Now()
		if m.Frames == 0 {
			m.first = now
		}
		m.times[m.Frames%int64(len(m.times))] = now.UnixNano()
		m.Frames++
		m.Last = Write{Events: ev2 - ev, Wire: wire2 - wire, At: now}
	}
	return err
}
