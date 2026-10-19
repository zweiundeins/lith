package live

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/zweiundeins/lith/internal/shutdown"
	"github.com/zweiundeins/lith/sse"
)

// Spec describes a stream.
type Spec struct {
	// ID names the stream for Watch and Unwatch, usually the tab id. A
	// stream with the ID of one still open replaces it if its request
	// came later, and else ends at once. Empty: none.
	ID string
	// Views are the views the stream watches from the start. Views that
	// change in the same tick are sent in this order.
	Views []View
	// Write sends a view's frame to the stream, and may tailor it (send
	// only what this stream has not seen). Nil: frames are []byte events,
	// written as they are. An error ends the stream.
	Write func(s *sse.Stream, key string, frame any) error
	// WriteTick, if set, takes Write's place: it gets the new frames of
	// the stream's views at once, so that one s.Write compresses and
	// flushes them together, where each call of Write is a write of its
	// own. Each call has the newest frame of every view that changed
	// since the last one, in the order the stream watches them: one
	// tick's, or several ticks' when the stream fell behind. frames is
	// valid during the call only. An error ends the stream.
	WriteTick func(s *sse.Stream, frames []Frame) error
	// Reconnect is the id of the page's element whose data-init opens this
	// stream with retry 'error'. When the hub stops (a deploy), the stream
	// ends with sse.Reconnect, so the page opens it again on the next
	// process. So it does when the streams' budget has room for a better
	// tier than the stream holds, which a brotli stream takes only on a new
	// connection (sse.Stream.Upgrade): once a minute at most for the same
	// ID, and for a stream without an ID only after a minute open. Empty:
	// the stream just ends, as it does when a newer stream with its ID
	// replaces it.
	Reconnect string
	// Slow marks a stream to a client on a slow link (edge.SlowLink tells
	// one). It sends the newest frame of a view 250 ms or more after the
	// last, four a second. A frame is due at once when it is its view's
	// first, or its view sent nothing for 250 ms, and it takes the frames
	// that wait along: views that change together still arrive together,
	// and a view that changes now and then is not held up, for one frame
	// more of the others with each of its own. While a frame waits, a view
	// that no other stream keeps up with is not rendered
	// (Stats.RendersDeferred), so it renders about four times a second,
	// and its frame can be up to 250 ms old when it goes out; a view that
	// others keep up with renders for them, and the frames in between drop
	// (Stats.FramesDropped). Heartbeats go out as before.
	Slow bool
	// Window, Ack, Release and ReleaseSlack pace a stream by its page.
	// A stream with Ack alone is paced as lith's template paces it: it
	// starts unpaced, Hub.Pace paces it once its page reports that it is
	// behind, and the defaults of Release and ReleaseSlack end the pacing
	// once the page keeps up again.
	//
	// Window, if positive, paces the stream by the page: at most Window
	// writes of frames go out that the page has not acknowledged with
	// Hub.Ack. Each write of frames ends with the event Ack makes for it,
	// which has the page acknowledge the write once it has applied it.
	// While Window writes wait for theirs, the stream's frames wait too:
	// a view that no other stream keeps up with is not rendered
	// (Stats.RendersDeferred), and a view that others keep up with drops
	// the frames in between (Stats.FramesDropped). When an
	// acknowledgement frees the window, a waiting frame whose view was
	// deferred since is dropped, and the next tick renders the view as it
	// is then, so the page never gets a frame that waited for it. A
	// stream that hears nothing for AckTimeout sends its next write
	// anyway. It needs an ID and Ack. A stream with Ack and no Window
	// starts unpaced, and Hub.Pace can pace it later.
	Window int
	// Ack makes the event that ends a write of frames on a stream with
	// Window. The event has the page call Hub.Ack with the stream's ID,
	// token and n once it has applied the write: n counts the stream's
	// paced writes of frames from 1, and token tells this stream from an
	// older or newer one with the same ID. It goes out as the last Frame
	// of WriteTick's call, with the key AckKey, or the last event of the
	// write.
	Ack func(token string, n int64) []byte
	// Release ends the pacing of a stream whose page keeps up: once no
	// acknowledgement for Release came after the stream's next frame was
	// ready (that is, the page was never the reason a frame waited), the
	// stream stops asking for them and sends every frame again
	// (Stats.Releases). Each release of the same stream doubles the time
	// for the next, up to eight times Release, so that a page at the edge
	// of keeping up, paced again by Hub.Pace, does not switch back and
	// forth every Release. 0 takes DefaultRelease; a negative Release
	// keeps the stream paced until Hub.Pace sets its window to 0.
	Release time.Duration
	// ReleaseSlack changes what Release counts as late once the page has
	// reported its round trip (Hub.RoundTrip): an acknowledgement that
	// came more than ReleaseSlack after the round trip, from the write it
	// acknowledges, instead of one that came after the next frame was
	// ready. A page on a link whose round trip is longer than a view's
	// tick can then still be released once it applies its frames within
	// ReleaseSlack. 0 takes DefaultReleaseSlack; a negative slack keeps
	// the rule of the next frame even when the page reports its round
	// trip. Until the page reports one, the rule of the next frame holds.
	ReleaseSlack time.Duration
}

// AckKey is the key of the Frame that carries a stream's acknowledgement
// event (Spec.Ack) to WriteTick or Write.
const AckKey = "live.ack"

// AckTimeout is how long a stream with Spec.Window waits for an
// acknowledgement before it sends its next write anyway.
const AckTimeout = 10 * time.Second

// DefaultReleaseSlack is Spec.ReleaseSlack when it is 0: a page that
// applies a frame within 100 ms of its round trip keeps up with ten frames
// a second.
const DefaultReleaseSlack = 100 * time.Millisecond

// DefaultRelease is Spec.Release when it is 0: a paced page whose
// acknowledgements all come in time for 5 s is released.
const DefaultRelease = 5 * time.Second

// behindEvery is the least time between two reports of a stream's page
// that Hub.Behind counts. newest-frame.js reports every 3 s at most.
const behindEvery = 2 * time.Second

// Frame is a view's new frame, for Spec.WriteTick.
type Frame struct {
	Key   string
	Value any
}

var (
	// ErrFull is returned by Serve when MaxStreams streams are open.
	ErrFull = errors.New("live: too many streams")
	// ErrStopped is returned by Serve once the hub has stopped.
	ErrStopped = errors.New("live: hub stopped")
)

var heartbeat = sse.Comment("")

// reopenAfter is how long a stream ID waits after the budget last asked
// its stream to open again before its next stream may be asked.
const reopenAfter = time.Minute

// errReopen ends a stream that its budget asked to open again.
var errReopen = errors.New("live: the budget has a better tier for the stream")

// slowEvery is the least time between two frames of a view on a stream
// with Spec.Slow: four frames a second.
const slowEvery = 250 * time.Millisecond

// Serve streams the views of spec to the client until it leaves or the
// hub stops, and then returns nil. It sends each view's newest frame when
// the hub keeps it (View.Render), else a frame rendered in the next tick,
// then every new frame. It opens the stream once those first frames are
// there, or the tick that renders them has ended, and passes their length
// to the streams' budget (sse.Options.Frame), so the response's headers go
// out with the first frames: a budget with sse.BudgetOptions.Refuse lets
// the stream in only with room for a window that holds them. When the hub
// is full or stopped, or the streams' budget refuses the stream, it
// answers 503 and returns ErrFull, ErrStopped or sse.ErrNoRoom; a failed
// write ends the stream with its error.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, spec Spec) error {
	for _, v := range spec.Views {
		check(v)
	}
	seq := h.seq.Add(1)
	h.mu.Lock()
	switch {
	case h.done.Err() != nil:
		h.mu.Unlock()
		http.Error(w, "the server is restarting", http.StatusServiceUnavailable)
		return ErrStopped
	case h.streams >= h.maxStreams:
		h.mu.Unlock()
		http.Error(w, "too many streams", http.StatusServiceUnavailable)
		return ErrFull
	}
	h.streams++
	h.mu.Unlock()

	ctx, cancelCause := context.WithCancelCause(r.Context())
	cancel := func() { cancelCause(nil) }
	defer cancel()
	defer context.AfterFunc(h.done, func() { cancelCause(shutdown.Cause) })()
	s := &stream{seq: seq, id: spec.ID, write: spec.Write, writeTick: spec.WriteTick, cancel: cancel, wake: make(chan struct{}, 1), upgrade: -1}
	if spec.Reconnect != "" {
		s.upgrade = h.upgradeAfter(spec.ID)
	}
	if spec.Slow {
		s.every = slowEvery
	}
	if spec.Ack != nil && spec.ID != "" {
		s.ack, s.release, s.slack = spec.Ack, spec.Release, spec.ReleaseSlack
		if s.release == 0 {
			s.release = DefaultRelease
		}
		if s.slack == 0 {
			s.slack = DefaultReleaseSlack
		}
		if spec.Window > 0 {
			s.pace(int64(spec.Window), time.Now())
		}
	}
	published := h.attach(s, spec.Views)
	o := h.sse
	o.Frame = h.first(ctx, s, published)
	st, err := sse.Open(w, r.WithContext(ctx), o)
	if err != nil {
		h.detach(s, errors.Is(err, sse.ErrNoRoom), spec.Views)
		switch {
		case r.Context().Err() != nil:
			return nil
		case h.done.Err() != nil:
			w.Header().Del("Content-Encoding")
			http.Error(w, "the server is restarting", http.StatusServiceUnavailable)
			return ErrStopped
		case ctx.Err() != nil:
			// A stream with the ID that came later replaced this one.
			return nil
		case errors.Is(err, sse.ErrNoRoom):
			http.Error(w, "too many streams", http.StatusServiceUnavailable)
		}
		return err
	}
	var last [][]byte
	defer func() { st.CloseWith(last...) }()
	defer h.detach(s, false, nil)
	err = h.pump(ctx, s, st)
	if (h.done.Err() != nil || errors.Is(err, errReopen)) && spec.Reconnect != "" && r.Context().Err() == nil {
		last = append(last, sse.Reconnect(spec.Reconnect))
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, errReopen) || h.done.Err() != nil {
		return nil
	}
	return err
}

// upgradeAfter is how long a stream with the ID waits before its budget
// may ask it to open again: none, unless the budget asked a stream with
// that ID within reopenAfter, or the stream has no ID.
func (h *Hub) upgradeAfter(id string) time.Duration {
	if id == "" {
		return reopenAfter
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return max(0, reopenAfter-time.Since(h.asked[id]))
}

// reopened notes that the budget asked the stream with the ID to open
// again, and forgets the IDs asked longer ago than reopenAfter.
func (h *Hub) reopened(id string) {
	if id == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for k, t := range h.asked {
		if now.Sub(t) >= reopenAfter {
			delete(h.asked, k)
		}
	}
	h.asked[id] = now
}

// attach makes s watch the views, and those Watch kept for its ID. It
// returns a channel closed once the tick that renders them has published
// its frames.
func (h *Hub) attach(s *stream, views []View) <-chan struct{} {
	h.mu.Lock()
	published := h.next
	if s.id != "" {
		old := h.byID[s.id]
		if old != nil && old.seq > s.seq {
			h.mu.Unlock()
			s.cancel()
			return published
		}
		if old != nil {
			old.cancel()
		}
		h.byID[s.id] = s
	}
	marked := false
	for _, v := range views {
		marked = h.watch(s, v) || marked
	}
	if ps := h.pending[s.id]; ps != nil {
		delete(h.pending, s.id)
		now := time.Now()
		for _, p := range ps {
			if now.Before(p.until) {
				marked = h.watch(s, p.def) || marked
			}
		}
	}
	h.mu.Unlock()
	if marked {
		h.poke()
	}
	return published
}

// detach takes s off the hub. When the budget refused s, the views that
// Watch gave it besides those of its spec go to the stream open with its
// ID, or wait for the next one, as they would have if s had never opened.
func (h *Hub) detach(s *stream, refused bool, spec []View) {
	h.mu.Lock()
	if s.id != "" && h.byID[s.id] == s {
		delete(h.byID, s.id)
	}
	marked := false
	now := time.Now()
	for _, w := range s.watches {
		if refused && s.id != "" && !slices.ContainsFunc(spec, func(v View) bool { return v.Key == w.v.key }) {
			v := w.v
			def := View{Key: v.key, Topics: v.topics, Render: v.render, Every: v.every, Presence: v.presence}
			if o := h.byID[s.id]; o != nil {
				marked = h.watch(o, def) || marked
			} else {
				h.pend(s.id, def, now)
			}
		}
		marked = h.drop(w) || marked
	}
	s.mu.Lock()
	s.watches = nil
	s.mu.Unlock()
	h.streams--
	h.mu.Unlock()
	if marked {
		h.poke()
	}
}

// first waits for the first frames of the views s watches, so that Open
// knows the length of its first write: until each view has offered one,
// or published closes, or a heartbeat has passed. A hub that does not run
// renders nothing, and it does not wait. It returns the length of the
// frames offered by then.
func (h *Hub) first(ctx context.Context, s *stream, published <-chan struct{}) int {
	if n, all := s.offered(); all || !h.running.Load() {
		return n
	}
	timeout := time.NewTimer(h.heartbeat)
	defer timeout.Stop()
	for {
		select {
		case <-s.wake:
			if n, all := s.offered(); all {
				return n
			}
			continue
		case <-published:
		case <-timeout.C:
		case <-ctx.Done():
			return 0
		}
		n, _ := s.offered()
		return n
	}
}

// offered adds up the length of the frames offered to s and not sent yet,
// and reports whether each view it watches has one.
func (s *stream) offered() (n int, all bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all = true
	for _, w := range s.watches {
		if w.next == nil {
			all = false
			continue
		}
		n += max(0, frameLen(w.next.value))
	}
	return n, all
}

// hold is how long the stream's new frames wait at now: until one of them
// is due, a view's first or one whose view sent nothing for s.every. It is
// 0 when one is due, or when there is none. s.mu is held.
func (s *stream) hold(now time.Time) time.Duration {
	if s.every <= 0 {
		return 0
	}
	wait := time.Duration(-1)
	for _, w := range s.watches {
		if w.next == nil {
			continue
		}
		if w.sent.IsZero() {
			return 0
		}
		left := s.every - now.Sub(w.sent)
		if left <= 0 {
			return 0
		}
		if wait < 0 || left < wait {
			wait = left
		}
	}
	return max(wait, 0)
}

// unwatched reports whether the stream stopped watching w's view.
func (s *stream) unwatched(w *watch) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return w.gone
}

// pump sends the stream's new frames as they come, or with Spec.Slow when
// one is due (s.hold), and a heartbeat when it has been silent for
// h.heartbeat, or every h.heartbeat with a heartbeat signal. It returns
// errReopen when the stream's budget asks it to open again (s.upgrade).
func (h *Hub) pump(ctx context.Context, s *stream, st *sse.Stream) error {
	idle := time.NewTimer(h.heartbeat)
	defer idle.Stop()
	var due <-chan time.Time // fires when a frame that waits is due
	var dueTimer *time.Timer
	if s.every > 0 {
		dueTimer = time.NewTimer(time.Hour)
		dueTimer.Stop()
		defer dueTimer.Stop()
	}
	var upgrade <-chan struct{} // closed when the budget asks the stream to open again
	var listen <-chan time.Time // fires when the stream starts to listen for that
	if s.upgrade == 0 {
		upgrade = st.Upgrade()
	} else if s.upgrade > 0 {
		t := time.NewTimer(s.upgrade)
		defer t.Stop()
		listen = t.C
	}
	var rest <-chan time.Time // fires when the stream should hibernate
	var restTimer *time.Timer
	asleep := false
	if h.hibernate > 0 {
		restTimer = time.NewTimer(h.hibernate)
		defer restTimer.Stop()
		rest = restTimer.C
	}
	var stall <-chan time.Time // fires when a full window has waited AckTimeout
	var stallTimer *time.Timer
	if s.ack != nil {
		stallTimer = time.NewTimer(time.Hour)
		stallTimer.Stop()
		defer stallTimer.Stop()
	}
	token := strconv.FormatUint(s.seq, 36)
	var batch, stale []offer
	var parts [][]byte
	var frames []Frame
	for {
		var now time.Time
		if s.every > 0 || s.ack != nil {
			now = time.Now()
		}
		s.mu.Lock()
		wait := s.hold(now)
		window := s.window // Hub.Pace and Hub.Ack change it
		full := window > 0 && s.written-s.acked >= window && now.Sub(s.heard) < AckTimeout
		freed := s.full && !full
		s.full = full
		if wait == 0 && !full {
			for _, w := range s.watches {
				if w.next == nil {
					continue
				}
				// A frame that waited for the window while its view's
				// render was deferred is older than the view: the next
				// tick renders a newer one.
				if freed && w.v.deferred.Load() {
					stale = append(stale, offer{w, w.next})
					w.next = nil
					continue
				}
				batch = append(batch, offer{w, w.next})
				w.next, w.sent = nil, now
			}
		}
		var heard time.Time
		if full {
			heard = s.heard
		}
		s.mu.Unlock()
		h.resume(batch)
		if len(stale) > 0 {
			h.dropped.Add(int64(len(stale)))
			h.resume(stale)
			clear(stale)
			stale = stale[:0]
		}
		due = nil
		if wait > 0 {
			dueTimer.Reset(wait)
			due = dueTimer.C
		}
		stall = nil
		if full {
			stallTimer.Reset(AckTimeout - now.Sub(heard))
			stall = stallTimer.C
		}

		_, wire := st.Sent()
		wrote := false
		for _, o := range batch {
			if s.unwatched(o.w) {
				continue
			}
			if s.writeTick != nil {
				frames = append(frames, Frame{Key: o.w.v.key, Value: o.f.value})
				continue
			}
			if s.write != nil {
				if err := s.write(st, o.w.v.key, o.f.value); err != nil {
					return err
				}
				h.sent.Add(1)
				wrote = true
				continue
			}
			b, ok := o.f.value.([]byte)
			if !ok {
				h.log.Error("live: a frame is not []byte and the stream has no Write", "view", o.w.v.key, "type", fmt.Sprintf("%T", o.f.value))
				continue
			}
			parts = append(parts, b)
		}
		clear(batch)
		batch = batch[:0]
		if window > 0 && (len(frames) > 0 || len(parts) > 0 || wrote) {
			s.mu.Lock()
			s.written++
			n := s.written
			s.wroteAt[n%int64(len(s.wroteAt))] = now
			if n-s.acked >= window {
				s.heard = now // the window is full from now
			}
			s.mu.Unlock()
			ev := s.ack(token, n)
			switch {
			case len(frames) > 0:
				frames = append(frames, Frame{Key: AckKey, Value: ev})
			case len(parts) > 0:
				parts = append(parts, ev)
			default:
				if err := s.write(st, AckKey, ev); err != nil {
					return err
				}
			}
		}
		if len(frames) > 0 {
			n := len(frames)
			if window > 0 {
				n--
			}
			err := s.writeTick(st, frames)
			clear(frames)
			frames = frames[:0]
			if err != nil {
				return err
			}
			h.sent.Add(int64(n))
		}
		if len(parts) > 0 {
			n := len(parts)
			if window > 0 {
				n--
			}
			err := st.Write(parts...)
			clear(parts)
			parts = parts[:0]
			if err != nil {
				return err
			}
			h.sent.Add(int64(n))
		}
		if _, now := st.Sent(); now != wire {
			if h.ping == nil {
				idle.Reset(h.heartbeat)
			}
			if restTimer != nil {
				restTimer.Reset(h.hibernate)
				asleep = false
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
		case <-due:
		case <-stall:
		case <-idle.C:
			if err := st.Write(h.beat()); err != nil {
				return err
			}
			if asleep {
				if err := st.Hibernate(); err != nil {
					return err
				}
			}
			idle.Reset(h.heartbeat)
		case <-rest:
			if err := st.Hibernate(); err != nil {
				return err
			}
			asleep = true
		case <-listen:
			upgrade = st.Upgrade()
		case <-upgrade:
			h.reopened(s.id)
			return errReopen
		}
	}
}

// Behind counts a report that the page of the open stream with the ID fell
// behind it, as newest-frame.js tells with newest-frame-behind, in
// Stats.Behind, and the stream's first report in Stats.BehindStreams. It
// counts one report per stream every 2 s at most, so a page that sends
// more cannot inflate the counts, and none for an ID with no open stream.
// It reports whether it counted.
func (h *Hub) Behind(id string) bool {
	h.mu.Lock()
	s := h.byID[id]
	h.mu.Unlock()
	if s == nil {
		return false
	}
	now := time.Now()
	s.mu.Lock()
	first := s.behindAt.IsZero()
	counted := first || now.Sub(s.behindAt) >= behindEvery
	if counted {
		s.behindAt = now
	}
	s.mu.Unlock()
	if counted {
		h.behind.Add(1)
	}
	if first {
		h.behindStreams.Add(1)
	}
	return counted
}

// Ack acknowledges the writes of the stream with the ID and token up to
// the nth (Spec.Window), and frees the window for as many. It reports
// whether such a stream is open. An acknowledgement of an older write, or
// of one not written yet, changes nothing.
//
// With Spec.Release, it also notes whether a frame was waiting for the
// acknowledgement, and ends the pacing once none has for long enough.
//
// An app's route that calls Ack must check that the stream belongs to the request's session and limit how
// often a client may call it.
func (h *Hub) Ack(id, token string, n int64) bool {
	h.mu.Lock()
	s := h.byID[id]
	h.mu.Unlock()
	if s == nil || s.ack == nil || strconv.FormatUint(s.seq, 36) != token {
		return false
	}
	h.acks.Add(1)
	now := time.Now()
	s.mu.Lock()
	released := false
	if s.window > 0 && n > s.acked && n <= s.written {
		s.acked, s.heard = n, now
		if s.lateAck(n, now) {
			s.late = now
		} else if s.release > 0 && now.Sub(s.late) >= s.release<<min(s.releases, 3) {
			s.window = 0
			s.releases++
			released = true
		}
	}
	s.mu.Unlock()
	if released {
		h.releases.Add(1)
	}
	s.poke()
	return true
}

// Pace sets the window of the open stream with the ID (Spec.Window): a
// positive window paces it by the page's acknowledgements from its next
// write on, and 0 sends every frame again. The stream needs Spec.Ack. It
// reports whether such a stream is open. A stream paced already keeps its
// window and its count towards Spec.Release.
//
// An app's route that calls Pace must check that the stream belongs to the request's session and limit how
// often a client may call it.
func (h *Hub) Pace(id string, window int) bool {
	h.mu.Lock()
	s := h.byID[id]
	h.mu.Unlock()
	if s == nil || s.ack == nil {
		return false
	}
	s.mu.Lock()
	paced := false
	switch {
	case window <= 0:
		s.window = 0
	case s.window == 0:
		s.pace(int64(window), time.Now())
		paced = true
	}
	s.mu.Unlock()
	if paced {
		h.paces.Add(1)
	}
	s.poke()
	return true
}

// pace starts pacing s with the window at now: nothing it wrote before
// waits for an acknowledgement. s.mu is held, or s is not running yet.
func (s *stream) pace(window int64, now time.Time) {
	s.window, s.acked, s.heard, s.late = window, s.written, now, now
}

// lateAck reports whether the acknowledgement of write n, at now, counts
// against Spec.Release (s.mu held).
func (s *stream) lateAck(n int64, now time.Time) bool {
	if s.slack < 0 || s.rtt <= 0 {
		return s.waiting()
	}
	if s.written-n >= int64(len(s.wroteAt)) {
		return true
	}
	return now.Sub(s.wroteAt[n%int64(len(s.wroteAt))]) > s.rtt+s.slack
}

// RoundTrip notes a round trip the page of the open stream with the ID
// measured, such as from the Resource Timing of its last acknowledgement.
// The stream keeps the shortest (Spec.ReleaseSlack).
func (h *Hub) RoundTrip(id string, rtt time.Duration) {
	h.mu.Lock()
	s := h.byID[id]
	h.mu.Unlock()
	if s == nil || rtt <= 0 {
		return
	}
	s.mu.Lock()
	if s.rtt == 0 || rtt < s.rtt {
		s.rtt = rtt
	}
	s.mu.Unlock()
}

// waiting reports whether a frame waits to go out on s (s.mu held).
func (s *stream) waiting() bool {
	for _, w := range s.watches {
		if w.next != nil {
			return true
		}
	}
	return false
}

// beat is a heartbeat: a comment, or a patch of the heartbeat signal.
func (h *Hub) beat() []byte {
	if h.ping == nil {
		return heartbeat
	}
	b := strconv.AppendInt(slices.Clone(h.ping), h.pings.Add(1), 10)
	return sse.PatchSignals(append(b, '}'), false)
}
