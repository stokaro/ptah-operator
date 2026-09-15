-- The schema and the rows that migrations 1 and 2 produce between them, as one
-- file. The name is the contract: `.checkpoint.` is what makes Ptah treat this
-- as the floor a fresh database starts from instead of a fourth migration.
--
-- A database that has already run 1 and 2 ignores it. A database that has run
-- nothing runs this and then 4, and has to end up indistinguishable from one
-- that replayed the whole history -- which is the row this fixture exists for.
CREATE TABLE e2e_migration_widgets (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    color TEXT NOT NULL
);

INSERT INTO e2e_migration_widgets (id, name, color)
VALUES (1, 'first', 'unset'), (2, 'second', 'unset'), (3, 'third', 'unset');
