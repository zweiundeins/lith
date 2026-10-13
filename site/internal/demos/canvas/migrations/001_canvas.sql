-- The canvas: one row. cells packs the 64 x 64 palette indices two to a
-- byte, the first cell of each pair in the high nibble, so that its hex
-- is the board's cells attribute. version counts the changes to cells:
-- the encoded board is cached by it. epoch tells this database's canvas
-- from another's in the same process (tests run several).
CREATE TABLE canvas_board (
    id      INTEGER PRIMARY KEY CHECK (id = 1),
    epoch   INTEGER NOT NULL,
    version INTEGER NOT NULL,
    pixels  INTEGER NOT NULL, -- pixels painted since the start
    cells   BLOB NOT NULL     -- 2048 bytes
);

INSERT INTO canvas_board (id, epoch, version, pixels, cells)
VALUES (1, abs(random()), 1, 0, zeroblob(2048));

-- When each cell that is not blank was last painted: at most one row per
-- cell. The sweep fades the cells whose day is over, by the index.
CREATE TABLE canvas_painted (
    cell   INTEGER PRIMARY KEY, -- y * 64 + x
    minute INTEGER NOT NULL     -- unix minutes
);

CREATE INDEX canvas_painted_minute ON canvas_painted (minute);

-- A painter is a tab that picked a colour or painted. tag stands for the
-- tab in the views everyone shares; sid finds the colour a session used
-- last. The sweep deletes painters not seen for a day.
CREATE TABLE canvas_painters (
    id     INTEGER PRIMARY KEY,
    tag    TEXT NOT NULL UNIQUE,
    sid    TEXT NOT NULL,
    color  INTEGER NOT NULL,
    pixels INTEGER NOT NULL, -- painted by this tab
    seen   INTEGER NOT NULL  -- unix seconds of its last pick or paint
);

CREATE INDEX canvas_painters_sid ON canvas_painters (sid, seen);
CREATE INDEX canvas_painters_seen ON canvas_painters (seen);

-- The pixels each painter painted in each second of the last minute: a
-- paint adds to its second and deletes the seconds that have left the
-- minute, so the table holds at most 60 rows per painter.
CREATE TABLE canvas_recent (
    sec     INTEGER NOT NULL, -- unix seconds
    painter INTEGER NOT NULL, -- canvas_painters.id
    pixels  INTEGER NOT NULL,
    PRIMARY KEY (sec, painter)
) WITHOUT ROWID;
