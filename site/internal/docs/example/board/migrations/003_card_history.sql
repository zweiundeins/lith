-- Every title a card had: the card_history projector's table, filled from
-- the cards' streams in lith_events, which the events package creates.
CREATE TABLE card_history (
    card_id INTEGER NOT NULL,
    version INTEGER NOT NULL, -- the rename's version in the card's stream
    title   TEXT NOT NULL,
    at      INTEGER NOT NULL, -- unix seconds
    PRIMARY KEY (card_id, version)
) WITHOUT ROWID;
