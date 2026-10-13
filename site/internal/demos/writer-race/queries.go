package writerrace

import (
	"context"
	"encoding/json"
	"time"

	"github.com/zweiundeins/lith/sqlite"
)

// Race is a race as its row has it: its settings, where it is, and its
// numbers so far (Last.At is the time its producers have run).
type Race struct {
	ID int64
	Settings
	State                        string // queued, running, done, failed, interrupted, dropped
	QueuedAt, StartedAt, EndedAt time.Time
	Last                         Sample
}

// Over reports whether the race has ended, one way or another.
func (r Race) Over() bool { return r.State != "queued" && r.State != "running" }

// Rate is the race's commands a second over the time it ran.
func (r Race) Rate() float64 {
	if r.Last.At <= 0 {
		return 0
	}
	return float64(r.Last.Commands) / r.Last.At.Seconds()
}

// PerTx is the race's commands per transaction.
func (r Race) PerTx() float64 {
	if r.Last.Batches == 0 {
		return 0
	}
	return float64(r.Last.Commands) / float64(r.Last.Batches)
}

// Point is one sample of a race, as the charts read it.
type Point struct {
	At       time.Duration
	Commands int64
	Batches  int64
	Queue    int
	P99      time.Duration
}

const raceColumns = `id, producers, sync, state, queued_at, started_at, ended_at,
	elapsed_ms, commands, batches, failures, queue, max_batch, p99_us, sizes`

type scanner interface{ Scan(dest ...any) error }

func scanRace(s scanner) (Race, error) {
	var r Race
	var queued, started, ended, elapsed, p99 int64
	var sizes string
	err := s.Scan(&r.ID, &r.Producers, &r.Sync, &r.State, &queued, &started, &ended,
		&elapsed, &r.Last.Commands, &r.Last.Batches, &r.Last.Failures, &r.Last.Queue, &r.Last.MaxBatch, &p99, &sizes)
	if err != nil {
		return r, err
	}
	r.QueuedAt, r.StartedAt, r.EndedAt = time.UnixMilli(queued), millis(started), millis(ended)
	r.Last.At, r.Last.P99 = time.Duration(elapsed)*time.Millisecond, time.Duration(p99)*time.Microsecond
	if err := json.Unmarshal([]byte(sizes), &r.Last.Sizes); err != nil {
		return r, err
	}
	return r, nil
}

func millis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func races(ctx context.Context, q sqlite.Querier, query string, args ...any) ([]Race, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Race
	for rows.Next() {
		r, err := scanRace(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	return list, rows.Err()
}

// onTrack returns the race running now or, when none runs, the last one
// that ended with numbers; ID 0 when there has been none.
func onTrack(ctx context.Context, q sqlite.Querier) (Race, error) {
	list, err := races(ctx, q, `SELECT `+raceColumns+` FROM writer_race_races
		WHERE state IN ('running', 'done', 'failed', 'interrupted') ORDER BY state = 'running' DESC, id DESC LIMIT 1`)
	if err != nil || len(list) == 0 {
		return Race{}, err
	}
	return list[0], nil
}

// inLine returns the race on the track, if one runs, and the races that
// wait, first in line first: the one on the track joined the line before
// them all.
func inLine(ctx context.Context, q sqlite.Querier) ([]Race, error) {
	return races(ctx, q, `SELECT `+raceColumns+` FROM writer_race_races WHERE state IN ('running', 'queued') ORDER BY id`)
}

// recent returns the newest n races that ended, newest first.
func recent(ctx context.Context, q sqlite.Querier, n int) ([]Race, error) {
	return races(ctx, q, `SELECT `+raceColumns+` FROM writer_race_races
		WHERE state IN ('done', 'failed', 'interrupted') ORDER BY id DESC LIMIT ?`, n)
}

// raceOf returns the race with the id and, while it waits, its place in
// line (1: next); ok is false when there is no such race.
func raceOf(ctx context.Context, q sqlite.Querier, id int64) (r Race, place int, ok bool, err error) {
	list, err := races(ctx, q, `SELECT `+raceColumns+` FROM writer_race_races WHERE id = ?`, id)
	if err != nil || len(list) == 0 {
		return Race{}, 0, false, err
	}
	r = list[0]
	if r.State == "queued" {
		err = q.QueryRowContext(ctx, `SELECT count(*) FROM writer_race_races WHERE state = 'queued' AND id <= ?`, id).Scan(&place)
	}
	return r, place, true, err
}

// points returns the samples of a race, oldest first.
func points(ctx context.Context, q sqlite.Querier, race int64) ([]Point, error) {
	rows, err := q.QueryContext(ctx, `SELECT at_ms, commands, batches, queue, p99_us FROM writer_race_samples WHERE race_id = ? ORDER BY at_ms`, race)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Point
	for rows.Next() {
		var p Point
		var at, p99 int64
		if err := rows.Scan(&at, &p.Commands, &p.Batches, &p.Queue, &p99); err != nil {
			return nil, err
		}
		p.At, p.P99 = time.Duration(at)*time.Millisecond, time.Duration(p99)*time.Microsecond
		list = append(list, p)
	}
	return list, rows.Err()
}
