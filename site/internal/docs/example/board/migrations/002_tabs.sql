-- A tab's own state, one row per tab: the op id of its last action and
-- why that changed nothing, if it did not; the card whose panel is open.
CREATE TABLE tab_state (
    sid    TEXT NOT NULL,
    tab_id TEXT NOT NULL,
    data   TEXT NOT NULL,    -- JSON of TabState
    seen   INTEGER NOT NULL, -- unix seconds, for the sweep
    PRIMARY KEY (sid, tab_id)
) WITHOUT ROWID;

-- Preferences that outlive a page: one row per session.
CREATE TABLE prefs (
    sid     TEXT PRIMARY KEY,
    compact INTEGER NOT NULL, -- 1: cards without their details
    seen    INTEGER NOT NULL
) WITHOUT ROWID;
