-- What migrations 1 and 2 produce between them, as one file.
--
-- The name is the contract: `.checkpoint.` is what makes Ptah treat this as the
-- floor a fresh database starts from rather than a third migration. A database
-- that has already run 1 and 2 ignores it; a database that has run nothing runs
-- this and then 4, and ends up indistinguishable from one that replayed
-- everything.
--
-- The column is NOT NULL here because that is where migration 2 left it. A
-- checkpoint that recorded the column as it was created, before the backfill
-- and the constraint, would start a new database in a state the old one had
-- already left.
CREATE TABLE shipments (
    id INTEGER PRIMARY KEY,
    reference TEXT NOT NULL,
    carrier TEXT NOT NULL
);
