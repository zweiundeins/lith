-- The client address that started a race (edge.ClientKey), so that one
-- address, which can mint a session with every request, has PerAddress
-- races in line or on the track at most. Older races have none.
ALTER TABLE writer_race_races ADD COLUMN client TEXT NOT NULL DEFAULT '';
CREATE INDEX writer_race_races_client ON writer_race_races (client, state);
