CREATE TABLE boards (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL
);

CREATE TABLE lanes (
    id        INTEGER PRIMARY KEY,
    board_id  INTEGER NOT NULL REFERENCES boards (id),
    name      TEXT NOT NULL,
    max_cards INTEGER NOT NULL DEFAULT 50
);

CREATE TABLE cards (
    id       INTEGER PRIMARY KEY,
    lane_id  INTEGER NOT NULL REFERENCES lanes (id),
    title    TEXT NOT NULL,
    version  INTEGER NOT NULL DEFAULT 1, -- moves and renames add one
    assignee TEXT,
    archived INTEGER                     -- unix seconds, NULL while on the board
);
CREATE INDEX cards_lane ON cards (lane_id);

-- Mail to send, written in the transaction that makes it; the sender job
-- deletes a row once the mail is out.
CREATE TABLE outbox (
    id      INTEGER PRIMARY KEY,
    to_addr TEXT NOT NULL,
    subject TEXT NOT NULL,
    at      INTEGER NOT NULL -- unix seconds
);
