-- A cursor's place is now beside an anchor of the landing page's hero (a
-- line of the acrostic, the monolith, the lede, the buttons): x and y are
-- fractions of that anchor's width and height. Rows from before stay on
-- the floor, with x and y fractions of its size.
ALTER TABLE cursor_party_cursors ADD COLUMN a TEXT NOT NULL DEFAULT 'floor';
