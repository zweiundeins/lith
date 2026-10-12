CREATE TABLE entries (
    id      INTEGER PRIMARY KEY,
    name    TEXT NOT NULL,
    message TEXT NOT NULL,
    at      INTEGER NOT NULL  -- unix seconds
);

CREATE TABLE tab_state (
    sid    TEXT NOT NULL,
    tab_id TEXT NOT NULL,
    data   TEXT NOT NULL,     -- JSON of the tab's view state
    seen   INTEGER NOT NULL,  -- unix seconds, for the sweep
    PRIMARY KEY (sid, tab_id)
) WITHOUT ROWID;
