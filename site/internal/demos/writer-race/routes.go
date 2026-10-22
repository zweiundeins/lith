package writerrace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
)

func mount(k *demo.Kit) {
	e := &Engine{Dir: filepath.Dir(k.DB.Path), Duration: Duration, Every: Every, Log: k.Log}
	e.Clean()
	r := &runner{w: k.W, e: e, log: k.Log, pause: Pause, dwell: Dwell}
	// Races that the last process left on the track end as interrupted,
	// and those in line start.
	if err := k.W.Send(recovered(time.Now(), r.wake)); err != nil {
		k.Log.Warn("writer race: recover", "err", err)
	}
	k.Action("start", demo.Rate{Every: startEvery, Burst: startBurst}, func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Tab, Op   string
			Producers int
			Sync      string
		}
		if !demo.Decode(w, req, &body) {
			return
		}
		v, ok := demo.Who(req, body.Tab)
		if !ok || !demo.ValidID(body.Op) {
			w.WriteHeader(http.StatusBadRequest) // no page of ours sends it
			return
		}
		cmd := Start{Viewer: v, Op: body.Op, Settings: Settings{body.Producers, body.Sync}, At: time.Now(), Joined: r.wake}
		// A retried request (a flaky network) starts one race.
		k.Act(w, req, v, demo.Ack{"ack": body.Op}, writer.Once(Slug+":"+v.Session+":"+body.Op, cmd))
	})
}

// runner takes the races in line onto the track, one at a time. A race
// that joins the line wakes it; it runs races until the line is empty,
// with a pause after each that grows with the line, and stops.
type runner struct {
	w     *writer.Writer // the site's: the line, the samples, the results
	e     *Engine
	log   *slog.Logger
	pause time.Duration // after a race with none behind it
	dwell time.Duration // a race's least time in line

	mu          sync.Mutex
	busy, again bool
}

// wake makes the runner look at the line. It never blocks: it runs after
// a commit, on the site's writer.
func (r *runner) wake() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.again = true
	if !r.busy {
		r.busy = true
		go r.drain()
	}
}

// idle reports whether the runner has stopped.
func (r *runner) idle() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.busy
}

func (r *runner) drain() {
	for r.look() {
		err := r.safely(r.races)
		if errors.Is(err, writer.ErrClosed) {
			r.mu.Lock()
			r.busy = false // the site stops
			r.mu.Unlock()
			return
		}
		if err != nil {
			r.log.Error("writer race: the runner", "err", err)
		}
	}
}

// look reports whether a wake came since the last look. When none did,
// the runner stops, under the same lock, so that the next wake starts it
// again.
func (r *runner) look() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.again {
		r.busy = false
		return false
	}
	r.again = false
	return true
}

// safely turns a panic of fn into its error.
func (r *runner) safely(fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
		}
	}()
	return fn()
}

// races runs the races in line, one after the other, until none waits.
func (r *runner) races() error {
	for {
		n, err := r.w.DoValue(context.Background(), claim(time.Now(), r.dwell))
		switch {
		case err != nil:
			return err
		case n.Wait > 0:
			time.Sleep(n.Wait) // the first in line joined a moment ago
		case n.Race.ID == 0:
			return nil // nobody waits
		default:
			r.run(n.Race)
			// The host rests longer the more races wait: half a pause
			// more for each.
			time.Sleep(r.pause + time.Duration(n.Behind)*r.pause/2)
		}
	}
}

// run runs the race on the engine: each sample becomes a Record on the
// site's writer, sent without waiting, and the last one a Finish.
func (r *runner) run(race Race) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	last, err := r.e.Run(ctx, race.ID, race.Settings, func(s Sample) {
		switch err := r.w.Send(Record{Race: race.ID, Sample: s}); {
		case errors.Is(err, writer.ErrClosed):
			cancel() // the site stops: so does the race
		case err != nil:
			r.log.Warn("writer race: a sample", "race", race.ID, "err", err)
		}
	})
	failed := err != nil && ctx.Err() == nil
	if failed {
		r.log.Error("writer race failed", "race", race.ID, "err", err)
	}
	if err := r.w.Do(context.Background(), Finish{Race: race.ID, Last: last, Failed: failed, EndedAt: time.Now()}); err != nil && !errors.Is(err, writer.ErrClosed) {
		r.log.Error("writer race: finish", "race", race.ID, "err", err)
	}
}
