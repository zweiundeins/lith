CREATE TABLE spaces (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL
) STRICT;

CREATE TABLE pages (
    id         INTEGER PRIMARY KEY,
    space_id   INTEGER NOT NULL REFERENCES spaces (id),
    title      TEXT NOT NULL,
    restricted INTEGER NOT NULL DEFAULT 0 -- 1: only people with a grant read it
) STRICT;

CREATE TABLE grants (
    person  INTEGER NOT NULL,
    page_id INTEGER NOT NULL REFERENCES pages (id),
    PRIMARY KEY (person, page_id)
) STRICT, WITHOUT ROWID;
