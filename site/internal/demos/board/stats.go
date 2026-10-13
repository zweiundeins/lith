package board

import (
	"sync"
	"time"
)

// window is how many seconds the tracker remembers; rateOver and
// seriesOf are the seconds of the rate and of the sparkline.
const (
	window   = 60
	rateOver = 10
	seriesOf = 30
)

// tracker counts what the board's renders cost, in memory and per second
// for the last minute: the moves each render brought, the renders, and
// the frames they made, one for each stream that watched the board. The
// board's view records each of its renders; the stats view reads the
// numbers every second. A restart starts the counts over.
type tracker struct {
	mu      sync.Mutex
	seen    bool  // whether the board has rendered since the process started
	version int64 // its version at the last render
	last    Tick
	secs    [window]second
}

type second struct {
	at                     int64 // unix seconds
	moves, renders, frames int
}

// Tick is the last tick that moved a card: how many moves its one render
// of the board took in, and how many frames that render made.
type Tick struct {
	Moves, Frames int
	At            time.Time
}

// Numbers are what the stats view shows.
type Numbers struct {
	// Viewers is how many streams watch the board now.
	Viewers int
	Last    Tick
	// PerSecond is the board's frames a second over the last rateOver
	// seconds; Series the frames of each of the last seriesOf seconds,
	// the oldest first.
	PerSecond float64
	Series    []int
}

// track is the process's tracker.
var track tracker

// rendered records a render of the board at version, sent to viewers
// streams.
func (t *tracker) rendered(now time.Time, version int64, viewers int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	moves := 0
	if t.seen && version > t.version {
		moves = int(version - t.version)
	}
	t.seen, t.version = true, version
	if moves > 0 {
		t.last = Tick{Moves: moves, Frames: viewers, At: now}
	}
	s := t.at(now.Unix())
	s.moves += moves
	s.renders++
	s.frames += viewers
}

// at is the bucket of second sec, emptied if it held an older second.
func (t *tracker) at(sec int64) *second {
	s := &t.secs[sec%window]
	if s.at != sec {
		*s = second{at: sec}
	}
	return s
}

// numbers reads the counts at now, for a board that viewers watch.
func (t *tracker) numbers(now time.Time, viewers int) Numbers {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := Numbers{Viewers: viewers, Last: t.last, Series: make([]int, seriesOf)}
	sec := now.Unix()
	// Whole seconds only: the one under way would make the rate jump.
	frames := 0
	for i := 1; i <= seriesOf; i++ {
		s := t.secs[(sec-int64(i))%window]
		if s.at != sec-int64(i) {
			continue
		}
		n.Series[seriesOf-i] = s.frames
		if i <= rateOver {
			frames += s.frames
		}
	}
	n.PerSecond = float64(frames) / rateOver
	return n
}
