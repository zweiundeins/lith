package live

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/zweiundeins/lith/sse"
)

// Spec describes a stream.
type Spec struct {
	// ID names the stream for Watch and Unwatch, usually the tab id. A
	// stream with the ID of one still open replaces it. Empty: none.
	ID string
	// Views are the views the stream watches from the start. Views that
	// change in the same tick are sent in this order.
	Views []View
	// Write sends a view's frame to the stream, and may tailor it (send
	// only what this stream has not seen). Nil: frames are []byte events,
	// written as they are. An error ends the stream.
	Write func(s *sse.Stream, key string, frame any) error
}

var (
	// ErrFull is returned by Serve when MaxStreams streams are open.
	ErrFull = errors.New("live: too many streams")
	// ErrStopped is returned by Serve once the hub has stopped.
	ErrStopped = errors.New("live: hub stopped")
)

var heartbeat = sse.Comment("")

// Serve streams the views of spec to the client until it leaves or the
// hub stops, and then returns nil. It sends each view's current frame (a
// view without one renders in the next tick), then every new frame. When
// the hub is full or stopped, it answers 503 and returns ErrFull or
// ErrStopped; a failed write ends the stream with its error.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, spec Spec) error {
	for _, v := range spec.Views {
		check(v)
	}
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

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(h.done, cancel)()
	st, err := sse.Open(w, r.WithContext(ctx), h.sse)
	if err != nil {
		h.mu.Lock()
		h.streams--
		h.mu.Unlock()
		switch {
		case r.Context().Err() != nil:
			return nil
		case h.done.Err() != nil:
			http.Error(w, "the server is restarting", http.StatusServiceUnavailable)
			return ErrStopped
		}
		return err
	}
	defer st.Close()
	s := &stream{seq: h.seq.Add(1), id: spec.ID, write: spec.Write, cancel: cancel, wake: make(chan struct{}, 1)}
	h.attach(s, spec.Views)
	defer h.detach(s)
	err = h.pump(ctx, s, st)
	if errors.Is(err, context.Canceled) || h.done.Err() != nil {
		return nil
	}
	return err
}

func (h *Hub) attach(s *stream, views []View) {
	h.mu.Lock()
	if s.id != "" {
		if old := h.byID[s.id]; old != nil {
			old.cancel()
		}
		h.byID[s.id] = s
	}
	marked := false
	for _, v := range views {
		marked = h.watch(s, v) || marked
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

// pump sends the stream's new frames as they come, and a heartbeat when
// it has been silent for h.heartbeat.
func (h *Hub) pump(ctx context.Context, s *stream, st *sse.Stream) error {
	idle := time.NewTimer(h.heartbeat)
	defer idle.Stop()
	var batch []*watch
	var parts [][]byte
	for {
		s.mu.Lock()
		batch = batch[:0]
		for _, w := range s.watches {
			if w.pending {
				w.pending = false
				batch = append(batch, w)
			}
		}
		s.mu.Unlock()

		_, wire := st.Sent()
		n := 0
		for _, w := range batch {
			f := w.v.frame.Load()
			if f == nil || f.seq == w.sent {
				continue
			}
			w.sent = f.seq
			n++
			if s.write != nil {
				if err := s.write(st, w.v.key, f.value); err != nil {
					return err
				}
				continue
			}
			b, ok := f.value.([]byte)
			if !ok {
				h.log.Error("live: a frame is not []byte and the stream has no Write", "view", w.v.key, "type", fmt.Sprintf("%T", f.value))
				continue
			}
			parts = append(parts, b)
		}
		if len(parts) > 0 {
			err := st.Write(parts...)
			clear(parts)
			parts = parts[:0]
			if err != nil {
				return err
			}
		}
		h.sent.Add(int64(n))
		if _, now := st.Sent(); now != wire {
			idle.Reset(h.heartbeat)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
		case <-idle.C:
			if err := st.Write(heartbeat); err != nil {
				return err
			}
			idle.Reset(h.heartbeat)
		}
	}
}
