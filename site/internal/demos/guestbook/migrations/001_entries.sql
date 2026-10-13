CREATE TABLE guestbook_entries (
    id      INTEGER PRIMARY KEY,
    name    TEXT NOT NULL,
    message TEXT NOT NULL,
    at      INTEGER NOT NULL  -- unix seconds
);
