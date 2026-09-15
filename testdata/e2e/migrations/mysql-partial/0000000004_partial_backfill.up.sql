-- A migration that commits some of its statements and not the rest.
--
-- MySQL commits every DDL statement on its own, so the column survives the
-- failure that follows it whatever the transaction mode says. The directive is
-- written anyway, because the file has to mean the same thing on both engines.
-- +ptah no_transaction
ALTER TABLE e2e_migration_widgets ADD COLUMN weight INT;

INSERT INTO e2e_migration_widget_weights (widget_id, weight)
SELECT id, 1 FROM e2e_migration_widgets;
