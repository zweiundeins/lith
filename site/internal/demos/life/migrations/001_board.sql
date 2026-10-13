-- The board: one row. cells holds its 80 × 50 cells, one byte each, row
-- by row: 0 for a dead cell, 1 to 8 for a live one in a person's colour,
-- 9 for one of the soup's. The clock's Step command rewrites the row ten
-- times a second while anyone watches.
CREATE TABLE life_board (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    cells      BLOB    NOT NULL,
    generation INTEGER NOT NULL,
    stepped    INTEGER NOT NULL, -- unix ms of the newest generation
    rate       INTEGER NOT NULL, -- generations a second over the last second, in tenths
    rate_since INTEGER NOT NULL, -- unix ms: where that second began
    rate_from  INTEGER NOT NULL, -- the generation then
    quiet      INTEGER NOT NULL, -- unix ms since the board is empty or repeats itself; 0 while it lives
    recent     BLOB    NOT NULL, -- the hashes of the last four generations, 8 bytes each
    history    TEXT    NOT NULL, -- the population once a second, the last 60: a JSON array
    reset      INTEGER NOT NULL  -- unix ms of the last reset
);

-- An empty board, quiet since long ago: the first step seeds it.
INSERT INTO life_board VALUES (1, zeroblob(4000), 0, 0, 0, 0, 0, 1, x'', '[]', 0);

-- The colour a session picked. A session without a row draws in a colour
-- its id picks. The sweep deletes rows unseen for 30 days, and keeps the
-- newest 10,000.
CREATE TABLE life_players (
    id     INTEGER PRIMARY KEY,
    sid    TEXT    NOT NULL UNIQUE,
    colour INTEGER NOT NULL,
    seen   INTEGER NOT NULL -- unix seconds
);

CREATE INDEX life_players_seen ON life_players (seen);
