-- The state of each tab of a demo, written by the demo's commands: what
-- became of the tab's last action, a choice the page must keep across a
-- deploy. One row per demo, session and tab; the sweep deletes rows of
-- tabs gone quiet (internal/demo: SaveTab, LoadTab, SweepTabs).
CREATE TABLE site_tabs (
    demo   TEXT NOT NULL,
    sid    TEXT NOT NULL,
    tab_id TEXT NOT NULL,
    data   TEXT NOT NULL,    -- JSON of the tab's state
    seen   INTEGER NOT NULL, -- unix seconds of the last write, for the sweep
    PRIMARY KEY (demo, sid, tab_id)
) WITHOUT ROWID;

CREATE INDEX site_tabs_seen ON site_tabs (seen);
