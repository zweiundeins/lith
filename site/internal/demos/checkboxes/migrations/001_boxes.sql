-- A million boxes in 3,907 chunks of 256: box b is bit b % 8 of byte
-- (b % 256) / 8 in chunk b / 256. Every chunk exists from the start, all
-- boxes clear, so a command only ever updates a row. The last chunk holds
-- the 64 boxes from 999,937 on.
CREATE TABLE checkboxes_chunks (
    id   INTEGER PRIMARY KEY, -- the chunk's number, from 0
    bits BLOB NOT NULL        -- 32 bytes
);

WITH RECURSIVE n(id) AS (SELECT 0 UNION ALL SELECT id + 1 FROM n WHERE id < 3906)
INSERT INTO checkboxes_chunks (id, bits) SELECT id, zeroblob(32) FROM n;

-- One row: how many boxes are checked, and how many flips there were.
CREATE TABLE checkboxes_totals (
    id      INTEGER PRIMARY KEY CHECK (id = 1),
    checked INTEGER NOT NULL,
    flips   INTEGER NOT NULL
);

INSERT INTO checkboxes_totals (id, checked, flips) VALUES (1, 0, 0);

-- Flips per second, for the rate on the page. The sweep keeps an hour.
CREATE TABLE checkboxes_seconds (
    sec   INTEGER PRIMARY KEY, -- unix seconds
    flips INTEGER NOT NULL
);
