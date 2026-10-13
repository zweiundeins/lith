-- Every rename of a card as an event; cards.title is its projection.
CREATE TABLE card_events (
    id      INTEGER PRIMARY KEY,
    card_id INTEGER NOT NULL REFERENCES cards (id),
    type    TEXT NOT NULL,
    data    TEXT NOT NULL,   -- JSON
    at      INTEGER NOT NULL -- unix seconds
);
CREATE INDEX card_events_card ON card_events (card_id, id);
