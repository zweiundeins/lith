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
}

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

// Serve streams the views of spec to the client until it leaves or the
// hub stops, and then returns nil. It sends each view's newest frame when
// the hub keeps it (View.Render), else a frame rendered in the next tick,
// then every new frame. When the hub is full or stopped, or the streams'
// budget refuses the stream, it answers 503 and returns ErrFull,
// ErrStopped or sse.ErrNoRoom; a failed write ends the stream with its
// error.
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
	st, err := sse.Open(w, r.WithContext(ctx), h.sse)
	if err != nil {
		h.mu.Lock()
		h.streams--
		h.mu.Unlock()
		switch {
		case r.Context().Err() != nil:
			return nil
		case h.done.Err() != nil:
			w.Header().Del("Content-Encoding")
			http.Error(w, "the server is restarting", http.StatusServiceUnavailable)
			return ErrStopped
		case errors.Is(err, sse.ErrNoRoom):
			http.Error(w, "too many streams", http.StatusServiceUnavailable)
		}
		return err
	}
	var last [][]byte
	defer func() { st.CloseWith(last...) }()
	s := &stream{seq: seq, id: spec.ID, write: spec.Write, writeTick: spec.WriteTick, cancel: cancel, wake: make(chan struct{}, 1), upgrade: -1}
	if spec.Reconnect != "" {
		s.upgrade = h.upgradeAfter(spec.ID)
	}
	h.attach(s, spec.Views)
	defer h.detach(s)
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

func (h *Hub) attach(s *stream, views []View) {
	h.mu.Lock()
	if s.id != "" {
		old := h.byID[s.id]
		if old != nil && old.seq > s.seq {
			h.mu.Unlock()
			s.cancel()
			return
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
}

func (h *Hub) detach(s *stream) {
	h.mu.Lock()
	if s.id != "" && h.byID[s.id] == s {
		delete(h.byID, s.id)
	}
	marked := false
	for _, w := range s.watches {
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

// unwatched reports whether the stream stopped watching w's view.
func (s *stream) unwatched(w *watch) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return w.gone
}

// pump sends the stream's new frames as they come, and a heartbeat when
// it has been silent for h.heartbeat, or every h.heartbeat with a
// heartbeat signal. It returns errReopen when the stream's budget asks it
// to open again (s.upgrade).
func (h *Hub) pump(ctx context.Context, s *stream, st *sse.Stream) error {
	idle := time.NewTimer(h.heartbeat)
	defer idle.Stop()
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
	var batch []offer
	var parts [][]byte
	var frames []Frame
	for {
		s.mu.Lock()
		for _, w := range s.watches {
			if w.next != nil {
				batch = append(batch, offer{w, w.next})
				w.next = nil
			}
		}
		s.mu.Unlock()

		_, wire := st.Sent()
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
		if len(frames) > 0 {
			err := s.writeTick(st, frames)
			n := len(frames)
			clear(frames)
			frames = frames[:0]
			if err != nil {
				return err
			}
			h.sent.Add(int64(n))
		}
		if len(parts) > 0 {
			err := st.Write(parts...)
			n := len(parts)
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

// beat is a heartbeat: a comment, or a patch of the heartbeat signal.
func (h *Hub) beat() []byte {
	if h.ping == nil {
		return heartbeat
	}
	b := strconv.AppendInt(slices.Clone(h.ping), h.pings.Add(1), 10)
	return sse.PatchSignals(append(b, '}'), false)
}
