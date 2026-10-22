package live

import (
	"fmt"
	"runtime/debug"
	"slices"
)

// MaxNotices is how many notices a stream keeps waiting, queued and held:
// a Notify with a new key beyond it drops the oldest. Keys may come from a
// client (one per op key), and the cap keeps what one stream holds small.
const MaxNotices = 64

// note is a notice of Notify.
type note struct {
	key   string
	value any
}

// heldNote is a notice that waits for the tick numbered after to publish.
// bumped: it waited once more for a view whose frame was still to come.
type heldNote struct {
	note
	after  uint64
	bumped bool
}

// Notify sends a notice to the open stream with this ID: frame goes out
// once and the hub forgets it. It is written as a view's frame is, under
// key: through Spec.WriteTick or Spec.Write, or as []byte events. A notice
// tells the person what no command decided and no view can render from
// the database: the writer failed, a rate limit turned an action away.
// Put the op id of the action it answers in it, as the tab's view does
// for an outcome, so that the page's pending state ends.
//
// A notice waits for the renders pending for the stream's views when
// Notify is called, such as those of the commit just before it, and goes
// out after their frames, so a frame of that commit never overwrites it:
// in the same write as the frames of the tick it waited for, or in a later
// write when that tick brought the stream none. A render that no worker
// took holds it one tick more, once. A render that fails holds it no
// longer: the notice goes out without that view's frame, since the view
// renders again only when a commit touches its topics, which a database
// that fails may not see for a while. It can still come first when the stream lags so far that the view
// defers that render (Stats.RendersDeferred). With none pending, it goes
// with the stream's next write. Like a frame, it waits while the stream
// holds its frames back: up to 250 ms with Spec.Slow, and while a paced
// stream's window is full, up to AckTimeout.
//
// A stream keeps one notice per key, so give a kind of notice a fixed key,
// never the op id: a Notify with the key of a notice that has not gone
// out yet replaces it, in its place, and the last one wins. A stream keeps
// MaxNotices waiting at most: one with a new key beyond them drops the
// oldest, queued before held. Notify reports
// whether such a stream is open; when none is, it does nothing and keeps
// nothing, so an ID a client made up costs nothing.
//
// A notice is best effort: a stream that is not open, or ends before the
// notice goes out, never sends it, and a page that reconnects or reloads
// never sees it. A []byte notice still waiting when the hub stops goes out
// before the events of Options.Stopping. Render a notice into a region
// that one of the stream's views fills, such as the tab's outcome: that
// view's next frame replaces it.
func (h *Hub) Notify(streamID, key string, frame any) bool {
	if streamID == "" || frame == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.byID[streamID]
	if s == nil {
		return false
	}
	n := note{key: key, value: frame}
	var after uint64
	if h.handing > h.finished {
		after = h.handing // a tick hands out its frames
	}
	for _, w := range s.watches {
		switch {
		case w.v.dirty:
			after = h.started + 1 // the next tick renders it
		case w.v.rendering:
			after = max(after, h.started)
		}
	}
	if after <= h.finished {
		s.note(n)
		return true
	}
	s.holdNote(n, after)
	h.holding[s] = struct{}{}
	return true
}

// note adds n to the stream's notices, in place of one with its key, and
// wakes the stream.
func (s *stream) note(n note) {
	s.mu.Lock()
	if i := slices.IndexFunc(s.held, func(o heldNote) bool { return o.key == n.key }); i >= 0 {
		s.held[i].value = n.value // goes out when the one it replaces would
		s.mu.Unlock()
		return
	}
	if i := slices.IndexFunc(s.notes, func(o note) bool { return o.key == n.key }); i >= 0 {
		s.notes[i] = n
	} else {
		s.makeRoom()
		s.notes = append(s.notes, n)
	}
	s.mu.Unlock()
	s.poke()
}

// makeRoom drops the oldest notice when the stream keeps MaxNotices, for
// one with a new key: a queued one first, else a held one. s.mu is held.
func (s *stream) makeRoom() {
	if len(s.notes)+len(s.held) < MaxNotices {
		return
	}
	if len(s.notes) > 0 {
		s.notes = slices.Delete(s.notes, 0, 1)
	} else {
		s.held = slices.Delete(s.held, 0, 1)
	}
}

// holdNote keeps n until the tick numbered after has published, in place
// of a notice with its key. Hub.mu is held.
func (s *stream) holdNote(n note, after uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = slices.DeleteFunc(s.notes, func(o note) bool { return o.key == n.key })
	if i := slices.IndexFunc(s.held, func(o heldNote) bool { return o.key == n.key }); i >= 0 {
		s.held[i] = heldNote{note: n, after: max(after, s.held[i].after)}
		return
	}
	s.makeRoom()
	s.held = append(s.held, heldNote{note: n, after: after})
}

// takeHeld moves the held notices that waited for the tick numbered seq to
// those the stream sends with its next write, and reports whether it
// moved any. s.mu is held.
func (s *stream) takeHeld(seq uint64) bool {
	moved := false
	s.held = slices.DeleteFunc(s.held, func(o heldNote) bool {
		if o.after > seq {
			return false
		}
		if i := slices.IndexFunc(s.notes, func(n note) bool { return n.key == o.key }); i >= 0 {
			s.notes[i] = o.note
		} else {
			s.notes = append(s.notes, o.note)
		}
		moved = true
		return true
	})
	return moved
}

// lagging reports whether a view of s still owes its frame and the next
// tick renders it: no worker took its render. A view whose render failed
// is not dirty, and holds no notice. Hub.mu is held.
func (h *Hub) lagging(s *stream) bool {
	return slices.ContainsFunc(s.watches, func(w *watch) bool { return w.v.lagging && w.v.dirty })
}

// release hands the streams the notices that waited for the tick numbered
// seq, which has published; publish has given those of the streams that
// got frames already, in the same write. A stream with a view that no
// worker took keeps them one tick more, once. Hub.mu is held.
func (h *Hub) release(seq uint64) {
	h.finished = seq
	for s := range h.holding {
		lagging := h.lagging(s)
		s.mu.Lock()
		if lagging {
			for i, o := range s.held {
				if o.after <= seq && !o.bumped {
					s.held[i].after, s.held[i].bumped = h.started+1, true
				}
			}
		}
		moved := s.takeHeld(seq)
		left := len(s.held)
		s.mu.Unlock()
		if moved {
			s.poke()
		}
		if left == 0 {
			delete(h.holding, s)
		}
	}
}

// unsent takes the notices the stream has not sent, held ones included,
// as events for its last write: the []byte ones, in order. It drops the
// others.
func (h *Hub) unsent(s *stream) [][]byte {
	s.mu.Lock()
	notes := s.notes
	for _, o := range s.held {
		notes = append(notes, o.note)
	}
	s.notes, s.held = nil, nil
	s.mu.Unlock()
	var out [][]byte
	for _, n := range notes {
		if b, ok := n.value.([]byte); ok {
			out = append(out, b)
		} else {
			h.log.Warn("live: a notice that is not []byte is dropped as the hub stops", "key", n.key)
		}
	}
	return out
}

// stopEvents are Options.Stopping's events for the stream of spec, or
// none when it panics.
func (h *Hub) stopEvents(spec Spec) (events [][]byte) {
	defer func() {
		if p := recover(); p != nil {
			h.log.Error("live: Options.Stopping panicked", "stream", spec.ID, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			events = nil
		}
	}()
	return h.stopping(spec)
}
