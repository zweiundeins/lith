-- Every race: in line, on the track, or over. A race belongs to the
-- session that started it, so that a session has one in line at most.
CREATE TABLE writer_race_races (
    id         INTEGER PRIMARY KEY,
    sid        TEXT NOT NULL,
    producers  INTEGER NOT NULL,
    sync       TEXT NOT NULL,                  -- NORMAL or FULL
    state      TEXT NOT NULL,                  -- queued, running, done, failed, interrupted, dropped
    queued_at  INTEGER NOT NULL,               -- unix milliseconds
    started_at INTEGER NOT NULL DEFAULT 0,
    ended_at   INTEGER NOT NULL DEFAULT 0,
    -- The race's numbers so far, from its writer's Stats: every sample
    -- updates them, and the last one stays.
    elapsed_ms INTEGER NOT NULL DEFAULT 0,
    commands   INTEGER NOT NULL DEFAULT 0,
    batches    INTEGER NOT NULL DEFAULT 0,
    failures   INTEGER NOT NULL DEFAULT 0,
    queue      INTEGER NOT NULL DEFAULT 0,
    max_batch  INTEGER NOT NULL DEFAULT 0,
    p99_us     INTEGER NOT NULL DEFAULT 0,
    sizes      TEXT NOT NULL DEFAULT '[]'      -- JSON: transactions by size, in powers of two
);

CREATE INDEX writer_race_races_state ON writer_race_races (state, id);
CREATE INDEX writer_race_races_sid ON writer_race_races (sid, state);

-- The samples of a race, one every 50 ms while it runs: the charts.
CREATE TABLE writer_race_samples (
    race_id  INTEGER NOT NULL,
    at_ms    INTEGER NOT NULL, -- since the race's producers started
    commands INTEGER NOT NULL,
    batches  INTEGER NOT NULL,
    queue    INTEGER NOT NULL,
    p99_us   INTEGER NOT NULL,
    PRIMARY KEY (race_id, at_ms)
) WITHOUT ROWID;
