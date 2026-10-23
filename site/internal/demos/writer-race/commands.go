package writerrace

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// TabState is a tab's state in site_tabs: the op id of its last start,
// the race that start is about, and why it put no race in line, if it
// did not: Again when the session's own race was in line or on the track
// already (that race is Race), Refusal when the line was full.
type TabState struct {
	Op      string `json:"op,omitempty"`
	Race    int64  `json:"race,omitempty"`
	Again   bool   `json:"again,omitempty"`
	Refusal string `json:"refusal,omitempty"`
}

// Start puts a race in line for the viewer's session, unless the session
// has one in line or on the track already, its client address has
// PerAddress, or the line is full, and records the outcome in the tab's
// state.
type Start struct {
	Viewer demo.Viewer
	Op     string
	Settings
	At time.Time
	// Client is the client's address (edge.ClientKey); "" counts none.
	Client string
	// Joined runs once a race joined the line and the batch committed: it
	// wakes the runner.
	Joined func()
}

func (c Start) Validate() error {
	switch {
	case c.Viewer.Session == "" || c.Viewer.Tab == "" || c.Op == "":
		return errors.New("a race needs a session, a tab and an op")
	case !c.Settings.Valid():
		return errors.New("a race has 1, 50 or 200 producers, and NORMAL or FULL")
	}
	return nil
}

func (c Start) Apply(tx *writer.Tx) error {
	state := TabState{Op: c.Op}
	var mine int64
	err := tx.QueryRow(`SELECT id FROM writer_race_races WHERE sid = ? AND state IN ('queued', 'running') ORDER BY id LIMIT 1`,
		c.Viewer.Session).Scan(&mine)
	switch {
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	case err == nil:
		// The tab follows the session's race from now on.
		state.Race, state.Again = mine, true
	default:
		if c.Client != "" {
			var theirs int
			if err := tx.QueryRow(`SELECT count(*) FROM writer_race_races WHERE client = ? AND state IN ('queued', 'running')`,
				c.Client).Scan(&theirs); err != nil {
				return err
			}
			if theirs >= PerAddress {
				state.Refusal = fmt.Sprintf("%d races from your network are on their way already. Try again once one has run.", theirs)
				break
			}
		}
		var waiting int
		if err := tx.QueryRow(`SELECT count(*) FROM writer_race_races WHERE state = 'queued'`).Scan(&waiting); err != nil {
			return err
		}
		if waiting >= Waiting {
			state.Refusal = fmt.Sprintf("%d races wait already. Try again in a minute.", waiting)
			break
		}
		res, err := tx.Exec(`INSERT INTO writer_race_races (sid, client, producers, sync, state, queued_at) VALUES (?, ?, ?, ?, 'queued', ?)`,
			c.Viewer.Session, c.Client, c.Producers, c.Sync, c.At.UnixMilli())
		if err != nil {
			return err
		}
		if state.Race, err = res.LastInsertId(); err != nil {
			return err
		}
		tx.Touch(LineTopic)
		if c.Joined != nil {
			tx.AfterCommit(c.Joined)
		}
	}
	return demo.SaveTab(tx, Slug, c.Viewer, state, c.At)
}

// next is what claim found: the race it took onto the track and how many
// wait behind it, or how long the first in line still waits before it may
// go (Dwell), or neither when nobody waits.
type next struct {
	Race   Race
	Behind int
	Wait   time.Duration
}

// claim takes the race first in line onto the track once it has waited
// dwell (Dwell on the site), so that every tab sees it join the line
// before it leaves it, with Capped producers at most when Deep races or
// more wait behind it. The runner calls it between races, so a race still
// marked running is one that the process before this one left behind:
// claim ends it as interrupted first.
func claim(now time.Time, dwell time.Duration) func(tx *writer.Tx) (next, error) {
	return func(tx *writer.Tx) (next, error) {
		res, err := tx.Exec(`UPDATE writer_race_races SET state = 'interrupted', ended_at = ? WHERE state = 'running'`, now.UnixMilli())
		if err != nil {
			return next{}, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			tx.Touch(Topic, LiveTopic, ResultsTopic)
		}
		var r Race
		var queued int64
		err = tx.QueryRow(`SELECT id, producers, sync, queued_at FROM writer_race_races WHERE state = 'queued' ORDER BY id LIMIT 1`).
			Scan(&r.ID, &r.Producers, &r.Sync, &queued)
		if errors.Is(err, sql.ErrNoRows) {
			return next{}, nil
		}
		if err != nil {
			return next{}, err
		}
		if wait := time.UnixMilli(queued).Add(dwell).Sub(now); wait > 0 {
			return next{Wait: wait}, nil
		}
		var behind int
		if err := tx.QueryRow(`SELECT count(*) - 1 FROM writer_race_races WHERE state = 'queued'`).Scan(&behind); err != nil {
			return next{}, err
		}
		if behind >= Deep {
			r.Producers = min(r.Producers, Capped)
		}
		if _, err := tx.Exec(`UPDATE writer_race_races SET state = 'running', started_at = ?, producers = ? WHERE id = ?`, now.UnixMilli(), r.Producers, r.ID); err != nil {
			return next{}, err
		}
		r.State = "running"
		tx.Touch(LineTopic, Topic, LiveTopic)
		return next{Race: r, Behind: behind}, nil
	}
}

// Record stores a sample of the race on the track: a row for the charts,
// and the race's numbers so far. A race no longer running takes none.
type Record struct {
	Race   int64
	Sample Sample
}

func (c Record) Apply(tx *writer.Tx) error {
	ok, err := update(tx, c.Race, c.Sample)
	if err != nil || !ok {
		return err
	}
	s := c.Sample
	if _, err := tx.Exec(`INSERT OR REPLACE INTO writer_race_samples (race_id, at_ms, commands, batches, queue, p99_us) VALUES (?, ?, ?, ?, ?, ?)`,
		c.Race, s.At.Milliseconds(), s.Commands, s.Batches, s.Queue, s.P99.Microseconds()); err != nil {
		return err
	}
	tx.Touch(LiveTopic)
	return nil
}

// update writes a sample's numbers into the race's row, if it runs.
func update(tx *writer.Tx, race int64, s Sample) (bool, error) {
	sizes, err := json.Marshal(s.Sizes)
	if err != nil {
		return false, err
	}
	res, err := tx.Exec(`UPDATE writer_race_races SET elapsed_ms = ?, commands = ?, batches = ?, failures = ?, queue = ?, max_batch = ?, p99_us = ?, sizes = ?
		WHERE id = ? AND state = 'running'`,
		s.At.Milliseconds(), s.Commands, s.Batches, s.Failures, s.Queue, s.MaxBatch, s.P99.Microseconds(), string(sizes), race)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Finish ends the race on the track with its last numbers: done, or
// failed when its writer reported an error.
type Finish struct {
	Race    int64
	Last    Sample
	Failed  bool
	EndedAt time.Time
}

func (c Finish) Apply(tx *writer.Tx) error {
	ok, err := update(tx, c.Race, c.Last)
	if err != nil || !ok {
		return err
	}
	state := "done"
	if c.Failed {
		state = "failed"
	}
	if _, err := tx.Exec(`UPDATE writer_race_races SET state = ?, ended_at = ?, queue = 0 WHERE id = ?`, state, c.EndedAt.UnixMilli(), c.Race); err != nil {
		return err
	}
	tx.Touch(Topic, LiveTopic, ResultsTopic)
	return nil
}

// recovered ends the races that the process before this one left on the
// track, and wakes the runner if races wait: the site sends it when it
// starts.
func recovered(now time.Time, wake func()) writer.Func {
	return func(tx *writer.Tx) error {
		res, err := tx.Exec(`UPDATE writer_race_races SET state = 'interrupted', ended_at = ? WHERE state = 'running'`, now.UnixMilli())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			tx.Touch(Topic, LiveTopic, ResultsTopic)
		}
		var waiting int
		if err := tx.QueryRow(`SELECT count(*) FROM writer_race_races WHERE state = 'queued'`).Scan(&waiting); err != nil {
			return err
		}
		if waiting > 0 {
			tx.AfterCommit(wake)
		}
		return nil
	}
}

// sweep drops races that waited in line for Stale (a runner that stopped
// would leave them there), keeps the newest Kept races and the samples of
// the newest Charted.
func sweep(tx *writer.Tx, now time.Time) error {
	res, err := tx.Exec(`UPDATE writer_race_races SET state = 'dropped', ended_at = ? WHERE state = 'queued' AND queued_at < ?`,
		now.UnixMilli(), now.Add(-Stale).UnixMilli())
	if err != nil {
		return err
	}
	dropped, _ := res.RowsAffected()
	if dropped > 0 {
		tx.Touch(LineTopic, Topic)
	}
	// The races it deletes are the oldest, never among those the page
	// shows: no view changes.
	if _, err := demo.KeepNewest(tx, "writer_race_races", Kept); err != nil {
		return err
	}
	// The samples of the race on the track stay: it is among the newest.
	_, err = tx.Exec(`DELETE FROM writer_race_samples WHERE race_id < (
		SELECT coalesce(min(id), 0) FROM (SELECT id FROM writer_race_races ORDER BY id DESC LIMIT ?))`, Charted)
	return err
}
