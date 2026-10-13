-- The shared board: twelve cards in three lanes (0 to do, 1 doing, 2 done).
-- A card's position is its place in its lane, 0 at the top, kept dense by
-- the moves. Its version grows with every move of it, so a move can say
-- which version its mover saw. start_lane and start_pos are where the
-- sweep puts it back after a quiet hour.
CREATE TABLE board_cards (
    id         INTEGER PRIMARY KEY,
    title      TEXT NOT NULL,
    tag        TEXT NOT NULL,
    lane       INTEGER NOT NULL,
    pos        INTEGER NOT NULL,
    version    INTEGER NOT NULL DEFAULT 0,
    moved_by   TEXT NOT NULL DEFAULT '', -- who moved it last (an avatar's seed), '' since the start
    moved_at   INTEGER NOT NULL DEFAULT 0, -- unix seconds of that move, 0 since the start
    start_lane INTEGER NOT NULL,
    start_pos  INTEGER NOT NULL
);

-- The board as a whole, one row: its version, one more with every move and
-- every reset, and the last move.
CREATE TABLE board_meta (
    id        INTEGER PRIMARY KEY CHECK (id = 1),
    version   INTEGER NOT NULL,
    moved_at  INTEGER NOT NULL, -- unix seconds of the last move, 0 since the last reset
    last_card INTEGER NOT NULL, -- 0 since the last reset
    last_lane INTEGER NOT NULL,
    last_by   TEXT NOT NULL
);

INSERT INTO board_meta (id, version, moved_at, last_card, last_lane, last_by) VALUES (1, 0, 0, 0, 0, '');

INSERT INTO board_cards (id, title, tag, lane, pos, start_lane, start_pos) VALUES
    (1, 'Sketch the monolith', 'design', 0, 0, 0, 0),
    (2, 'Write the first command', 'server', 0, 1, 0, 1),
    (3, 'Pick a pixel font', 'design', 0, 2, 0, 2),
    (4, 'Tune the tick', 'server', 0, 3, 0, 3),
    (5, 'Explain the writer', 'docs', 0, 4, 0, 4),
    (6, 'Render a shared view', 'server', 1, 0, 1, 0),
    (7, 'Compress the frames', 'ops', 1, 1, 1, 1),
    (8, 'Draw the demo cards', 'design', 1, 2, 1, 2),
    (9, 'Open a stream', 'server', 2, 0, 2, 0),
    (10, 'Set up the sweep', 'ops', 2, 1, 2, 1),
    (11, 'Write the guestbook', 'docs', 2, 2, 2, 2),
    (12, 'Ship it', 'ops', 2, 3, 2, 3);

-- Who is on the board's page: a tab says hello when its stream opens. The
-- presence view shows the tabs whose stream is open now (the hub counts
-- them), one avatar per session. The sweep forgets tabs whose stream has
-- closed.
CREATE TABLE board_here (
    tab   TEXT PRIMARY KEY,
    who   TEXT NOT NULL,    -- the avatar's seed: a hash of the session, never the session
    since INTEGER NOT NULL, -- unix seconds of the tab's first hello
    at    INTEGER NOT NULL  -- unix seconds of its latest
) WITHOUT ROWID;
