-- A migration that commits some of its statements and not the rest.
--
-- The directive is what makes the case real rather than arranged: inside the
-- per-migration transaction PostgreSQL would roll the whole file back, and a
-- file that leaves nothing behind needs no recovery. Without it the column is
-- committed and the statement that follows fails, which is the state a person
-- has to be told about.
-- +ptah no_transaction
ALTER TABLE e2e_migration_widgets ADD COLUMN weight INTEGER;

INSERT INTO e2e_migration_widget_weights (widget_id, weight)
SELECT id, 1 FROM e2e_migration_widgets;
