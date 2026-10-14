-- The trail: for each cell that died one or two generations ago, its age
-- times 16 plus the colour it had, else 0. The step writes it with cells;
-- an empty blob means no trail.
ALTER TABLE life_board ADD COLUMN trail BLOB NOT NULL DEFAULT x'';
