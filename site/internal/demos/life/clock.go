package life

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/writer"
)

// clock advances the board while anyone watches it. Every Idle it asks
// the hub how many streams watch the board; while some do, it sends a
// Step Speed times a second and waits for each to commit. It ends with the
// hub. Mount starts one for each site, with that site's hub and writer.
type clock struct {
	w   *writer.Writer
	hub *live.Hub
	log *slog.Logger
}

func (c *clock) run() {
	t := time.NewTicker(Idle)
	defer t.Stop()
	running := false
	var logged time.Time // a broken database fails every step: log once a minute
	for {
		select {
		case <-c.hub.Done():
			return
		case <-t.C:
		}
		if watched := c.hub.Count(BoardKey) > 0; watched != running {
			running = watched
			if running {
				t.Reset(time.Second / Speed)
			} else {
				t.Reset(Idle)
			}
		}
		if !running {
			continue
		}
		err := c.w.Do(context.Background(), Step{At: time.Now()})
		switch {
		case errors.Is(err, writer.ErrClosed):
			return
		case err != nil && time.Since(logged) > time.Minute:
			c.log.Warn("life: a step failed", "err", err)
			logged = time.Now()
		}
	}
}
