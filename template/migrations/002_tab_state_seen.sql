-- The hourly sweep deletes the tab state of tabs gone for a day: by this
-- index, rather than a scan of the table inside the writer's transaction.
CREATE INDEX tab_state_seen ON tab_state (seen);
