-- One row per tab at the party: where its pointer is, as fractions of the
-- party's width and height, and when it last moved. The key is a hash of
-- the tab's id: every page sees it, and no page learns another's tab id.
CREATE TABLE cursor_party_cursors (
    id   TEXT PRIMARY KEY, -- 12 hex digits of SHA-256 of the tab id
    sid  TEXT NOT NULL,    -- the session that moves it
    name TEXT NOT NULL,    -- "sage moth", from the id
    hue  INTEGER NOT NULL, -- 0 to 5, a colour of the site's palette, from the id
    x    REAL NOT NULL,    -- 0 to 1, of the party's width
    y    REAL NOT NULL,    -- 0 to 1, of its height
    seq  INTEGER NOT NULL, -- the page's count of its moves: an older move never wins
    born INTEGER NOT NULL, -- unix milliseconds of the first move
    at   INTEGER NOT NULL  -- unix milliseconds of the last move
) WITHOUT ROWID;

CREATE INDEX cursor_party_cursors_at ON cursor_party_cursors (at);

-- The moves of each second, for the counter and its sparkline: a row a
-- second while someone moves, swept after two minutes.
CREATE TABLE cursor_party_seconds (
    second INTEGER PRIMARY KEY, -- unix seconds
    moves  INTEGER NOT NULL
);
