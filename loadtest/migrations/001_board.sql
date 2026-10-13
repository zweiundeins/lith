CREATE TABLE loadtest_cards (
	id       INTEGER PRIMARY KEY,
	title    TEXT NOT NULL,
	labels   TEXT NOT NULL,
	due      TEXT NOT NULL,
	comments INTEGER NOT NULL,
	n        INTEGER NOT NULL DEFAULT 0
) STRICT;
