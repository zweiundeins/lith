package life

import (
	"errors"
	"fmt"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// Step is the clock's command, ten times a second while anyone watches:
// the board's next generation, or a new soup once the board has been quiet
// long enough.
type Step struct {
	At time.Time
}

func (c Step) Apply(tx *writer.Tx) error {
	b, err := load(tx.Context(), tx)
	if err != nil {
		return err
	}
	now := c.At.UnixMilli()
	dirty := false
	switch {
	case b.Quiet != 0 && now-b.Quiet >= b.quietFor().Milliseconds():
		b.reseed(now)
		dirty = true
	case Population(b.Cells) == 0:
		// An empty board stays as it is: no generation, no frame.
		if b.Quiet == 0 {
			b.Quiet, dirty = now, true
		}
		if b.Rate != 0 {
			b.Rate, dirty = 0, true
		}
	default:
		b.step(now)
		dirty = true
	}
	if !dirty {
		return nil
	}
	return b.save(tx)
}

// Draw draws a stroke: the cells on the lines through its points live in
// the colour of the session that drew them, or die when Alive is false.
type Draw struct {
	Viewer demo.Viewer
	Op     string
	Cells  []int
	Alive  bool
	At     time.Time
}

func (c Draw) Validate() error {
	if c.Viewer.Tab == "" || c.Op == "" {
		return errors.New("a stroke needs a tab and an op")
	}
	if len(c.Cells) == 0 || len(c.Cells) > MaxStroke {
		return fmt.Errorf("a stroke has 1 to %d points", MaxStroke)
	}
	for _, i := range c.Cells {
		if i < 0 || i >= Cells {
			return errors.New("a point is off the board")
		}
	}
	return nil
}

func (c Draw) Apply(tx *writer.Tx) error {
	b, err := load(tx.Context(), tx)
	if err != nil {
		return err
	}
	var colour byte
	if c.Alive {
		n, err := colourOf(tx.Context(), tx, c.Viewer.Session)
		if err != nil {
			return err
		}
		colour = byte(n)
	}
	from := c.Cells[0]
	for _, to := range c.Cells {
		Line(b.Cells, from, to, colour)
		from = to
	}
	b.changed(c.At.UnixMilli())
	if err := b.save(tx); err != nil {
		return err
	}
	return demo.SaveTab(tx, Slug, c.Viewer, TabState{Op: c.Op}, c.At)
}

// Drop stamps a pattern on the board in the colour of the session that
// dropped it, its anchor on Cell, turned clockwise by Turn quarters.
type Drop struct {
	Viewer  demo.Viewer
	Op      string
	Pattern string
	Cell    int
	Turn    int
	At      time.Time
}

func (c Drop) Validate() error {
	if c.Viewer.Tab == "" || c.Op == "" {
		return errors.New("a pattern needs a tab and an op")
	}
	if _, ok := Find(c.Pattern); !ok {
		return fmt.Errorf("there is no pattern %q", c.Pattern)
	}
	if c.Cell < 0 || c.Cell >= Cells || c.Turn < 0 || c.Turn > 3 {
		return errors.New("a pattern lands on the board, turned 0 to 3 quarters")
	}
	return nil
}

func (c Drop) Apply(tx *writer.Tx) error {
	b, err := load(tx.Context(), tx)
	if err != nil {
		return err
	}
	colour, err := colourOf(tx.Context(), tx, c.Viewer.Session)
	if err != nil {
		return err
	}
	p, _ := Find(c.Pattern)
	p.Stamp(b.Cells, c.Cell, c.Turn, byte(colour))
	b.changed(c.At.UnixMilli())
	if err := b.save(tx); err != nil {
		return err
	}
	return demo.SaveTab(tx, Slug, c.Viewer, TabState{Op: c.Op}, c.At)
}

// Reset clears the board, unless it is empty already or someone reset it
// less than ResetEvery ago: then only the tab hears why.
type Reset struct {
	Viewer demo.Viewer
	Op     string
	At     time.Time
}

func (c Reset) Validate() error {
	if c.Viewer.Tab == "" || c.Op == "" {
		return errors.New("a reset needs a tab and an op")
	}
	return nil
}

func (c Reset) Apply(tx *writer.Tx) error {
	b, err := load(tx.Context(), tx)
	if err != nil {
		return err
	}
	now := c.At.UnixMilli()
	state := TabState{Op: c.Op}
	switch since := time.Duration(now-b.Reset) * time.Millisecond; {
	case Population(b.Cells) == 0:
		state.Refusal = "The board is empty already."
	case since < ResetEvery:
		state.Refusal = fmt.Sprintf("Someone reset the board %s. Try again in %s.", ago(since), seconds(ResetEvery-since))
	default:
		b.Cells, b.Trail = make([]byte, Cells), nil
		b.restart()
		b.Reset = now
		b.changed(now)
		if err := b.save(tx); err != nil {
			return err
		}
	}
	return demo.SaveTab(tx, Slug, c.Viewer, state, c.At)
}

// seconds says a duration in whole seconds, rounded up, at least one:
// "4 s".
func seconds(d time.Duration) string {
	return fmt.Sprintf("%d s", max(1, int((d+time.Second-1)/time.Second)))
}

// ago says how long ago, in whole seconds rounded down: "4 s ago", or
// "just now" within the first second.
func ago(d time.Duration) string {
	if d < time.Second {
		return "just now"
	}
	return fmt.Sprintf("%d s ago", int(d/time.Second))
}

// Pick sets the colour a session draws in, and touches the session's
// topic, so that each of its tabs shows it.
type Pick struct {
	Viewer demo.Viewer
	Colour int
	At     time.Time
}

func (c Pick) Validate() error {
	if c.Viewer.Session == "" || c.Colour < 1 || c.Colour > Colours {
		return fmt.Errorf("a colour is 1 to %d", Colours)
	}
	return nil
}

func (c Pick) Apply(tx *writer.Tx) error {
	if _, err := tx.Exec(`INSERT INTO life_players (sid, colour, seen) VALUES (?, ?, ?)
		ON CONFLICT (sid) DO UPDATE SET colour = excluded.colour, seen = excluded.seen`,
		c.Viewer.Session, c.Colour, c.At.Unix()); err != nil {
		return err
	}
	tx.Touch(sessionTopic(c.Viewer.Session))
	return nil
}

// sweep forgets the colours of sessions unseen for PlayerTTL, and keeps
// the newest MaxPlayers. The board is one row and never grows.
func sweep(tx *writer.Tx, now time.Time) error {
	if _, err := demo.DeleteOlder(tx, "life_players", "seen", now.Add(-PlayerTTL)); err != nil {
		return err
	}
	_, err := demo.KeepNewest(tx, "life_players", MaxPlayers)
	return err
}
