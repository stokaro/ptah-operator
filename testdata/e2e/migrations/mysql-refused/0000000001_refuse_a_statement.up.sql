-- A migration the database refuses at its only statement.
--
-- Nothing commits, so the run reports Failed and not Partial. The revision row it
-- leaves is what stops the resource, and what the row this fixture serves is about.
SELECT count(*) FROM e2e_migration_no_such_table;
