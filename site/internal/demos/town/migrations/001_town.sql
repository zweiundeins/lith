-- The town: a grid of 12 by 8 tiles, each a stream of events in lith's
-- event log (lith_events, stream 'town.tile', keyed by the tile's number).
-- Two inline projectors keep the tables below, in the command that appends
-- the events.

-- town_tiles is the town now: a row per tile that has had an event, with
-- its kind ('' for grass), who changed it last (an avatar's seed, '' for
-- the town's own start) and its stream's version, which a page sends back
-- with a change.
CREATE TABLE town_tiles (
    tile    INTEGER PRIMARY KEY,
    kind    TEXT NOT NULL,
    who     TEXT NOT NULL,
    version INTEGER NOT NULL,
    at      INTEGER NOT NULL -- unix milliseconds of the last event
);

-- town_history is every tile's past: a row per thing that stood on a tile,
-- from the position of the event that put it there (valid_from) to the
-- position of the one that replaced or cleared it (valid_to, NULL while it
-- stands). The town as of position p is the rows with
-- valid_from <= p < valid_to. Grass has no row.
CREATE TABLE town_history (
    tile       INTEGER NOT NULL,
    kind       TEXT NOT NULL,
    who        TEXT NOT NULL,
    valid_from INTEGER NOT NULL,
    valid_to   INTEGER,
    PRIMARY KEY (tile, valid_from)
) WITHOUT ROWID;

-- The look at a position reads this index alone: the rows that started
-- before the position, with what they hold.
CREATE INDEX town_history_asof ON town_history (valid_from, valid_to, tile, kind, who);

-- One row: whether the starting town has been laid out.
CREATE TABLE town_meta (
    id     INTEGER PRIMARY KEY CHECK (id = 1),
    seeded INTEGER NOT NULL
);

INSERT INTO town_meta (id, seeded) VALUES (1, 0);
