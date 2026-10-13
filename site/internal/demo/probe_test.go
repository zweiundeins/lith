package demo

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sse"
)

// recordProbe reports what the meter held when it was asked, and an event
// of its own when it is told to.
type recordProbe struct {
	seen   []Meter
	frames []int
	say    bool
}

func (p *recordProbe) Report(st *sse.Stream, m *Meter, frames int) []byte {
	p.seen = append(p.seen, *m)
	p.frames = append(p.frames, frames)
	if !p.say {
		return nil
	}
	return sse.PatchSignals([]byte(`{"_x":1}`), false)
}

func open(t *testing.T) *sse.Stream {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
	r.Header.Set("Accept-Encoding", "zstd")
	st, err := sse.Open(httptest.NewRecorder(), r, sse.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// Probes write a tick's events in one write with the probes' reports, and
// count the stream's writes, its frames, its last frame and the heartbeats
// the hub sent in between.
func TestProbes(t *testing.T) {
	st := open(t)
	var ps Probes
	frame := sse.PatchElements([]byte(`<div id="x">hello</div>`))

	// Without a probe: the events as they are, nothing when there are none.
	if err := ps.Write(st, nil); err != nil {
		t.Fatal(err)
	}
	if ev, _ := st.Sent(); ev != 0 {
		t.Fatalf("an empty tick wrote %d bytes", ev)
	}
	if err := ps.Write(st, [][]byte{frame}); err != nil {
		t.Fatal(err)
	}

	p := &recordProbe{say: true}
	ps.Keep("probe", p)
	if err := ps.Write(st, [][]byte{frame, frame}); err != nil {
		t.Fatal(err)
	}
	// The meter counts the stream's writes from its first, the one before
	// the probe arrived too.
	if p.frames[0] != 2 || p.seen[0].Writes != 1 || p.seen[0].Last.Events != int64(len(frame)) || p.seen[0].Beats != 0 {
		t.Fatalf("the first report saw %d frames and %+v", p.frames[0], p.seen[0])
	}
	m := ps.m
	if m.Writes != 2 || m.Frames != 2 || m.Last.Events != int64(2*len(frame))+int64(len(sse.PatchSignals([]byte(`{"_x":1}`), false))) || m.Last.Wire <= 0 {
		t.Fatalf("after a write with a report: %+v", m)
	}

	// Two heartbeats, as the hub sends them, then a tick of the probe alone,
	// which reports alone.
	for range 2 {
		if err := st.Write(sse.Comment("")); err != nil {
			t.Fatal(err)
		}
	}
	if err := ps.Write(st, nil); err != nil {
		t.Fatal(err)
	}
	if got := p.seen[1]; got.Beats != 2 || p.frames[1] != 0 {
		t.Fatalf("after two heartbeats: %+v, %d frames", got, p.frames[1])
	}
	if ps.m.Writes != 3 || ps.m.Frames != 2 {
		t.Fatalf("a report alone counted as a frame: %+v", ps.m)
	}

	// A probe that has nothing to say: a tick without frames writes nothing.
	p.say = false
	before, _ := st.Sent()
	if err := ps.Write(st, nil); err != nil {
		t.Fatal(err)
	}
	if after, _ := st.Sent(); after != before || ps.m.Beats != 2 {
		t.Fatalf("a silent tick wrote %d bytes, beats %d", after-before, ps.m.Beats)
	}

	// The newest probe of a view replaces the one before.
	q := &recordProbe{}
	ps.Keep("probe", q)
	if err := ps.Write(st, [][]byte{frame}); err != nil {
		t.Fatal(err)
	}
	if len(p.seen) != 3 || len(q.seen) != 1 {
		t.Fatalf("the replaced probe was asked %d times, the new one %d", len(p.seen), len(q.seen))
	}
}

// Rate counts the frames over a window, or since the stream's first frame.
func TestMeterRate(t *testing.T) {
	var m Meter
	start := time.Unix(1_800_000_000, 0)
	at := func(i int) time.Time { return start.Add(time.Duration(i) * 250 * time.Millisecond) }
	frame := func(i int) {
		if m.Frames == 0 {
			m.first = at(i)
		}
		m.times[m.Frames%int64(len(m.times))] = at(i).UnixNano()
		m.Frames++
	}
	if r := m.Rate(start, 4*time.Second); r != 0 {
		t.Fatalf("no frames: %v a second", r)
	}
	for i := range 8 {
		frame(i)
	}
	// Eight frames since the first, two seconds ago.
	if r := m.Rate(at(8), 4*time.Second); r != 4 {
		t.Fatalf("young stream: %v a second", r)
	}
	for i := 8; i < 200; i++ {
		frame(i)
	}
	if r := m.Rate(at(200), 4*time.Second); r < 3.9 || r > 4.1 {
		t.Fatalf("four a second: %v", r)
	}
	// Quiet for ten seconds.
	if r := m.Rate(at(240), 4*time.Second); r != 0 {
		t.Fatalf("quiet: %v a second", r)
	}
	// Faster than the ring holds over the window: 20 a second.
	m = Meter{}
	for i := range 400 {
		if m.Frames == 0 {
			m.first = start
		}
		m.times[m.Frames%int64(len(m.times))] = start.Add(time.Duration(i) * 50 * time.Millisecond).UnixNano()
		m.Frames++
	}
	if r := m.Rate(start.Add(400*50*time.Millisecond), 4*time.Second); r < 19 || r > 21 {
		t.Fatalf("twenty a second: %v", r)
	}
}
