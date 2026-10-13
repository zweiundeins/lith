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
// Step Speed times a second and waits for each to commit, and when the
// last one leaves it sends one more, which records that no one watches.
// It ends with the hub.
type clock struct {
	w   *writer.Writer
	hub *live.Hub
	log *slog.Logger
}

func (c *clock) run() {
	t := time.NewTicker(Idle)
	defer t.Stop()
	watching := 0
	for {
		select {
		case <-c.hub.Done():
			return
		case <-t.C:
		}
		n := c.hub.Count(BoardKey)
		if n == 0 && watching == 0 {
			continue
		}
		switch {
		case watching == 0:
			t.Reset(time.Second / Speed)
		case n == 0:
			t.Reset(Idle)
		}
		watching = n
		err := c.w.Do(context.Background(), Step{At: time.Now(), Watching: n})
		switch {
		case errors.Is(err, writer.ErrClosed):
			return
		case err != nil:
			c.log.Warn("life: a step failed", "err", err)
		}
	}
}
