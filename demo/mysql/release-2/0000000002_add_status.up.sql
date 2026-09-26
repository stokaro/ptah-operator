-- Adds a status, fills it, and then constrains it. The constraint names a
-- column that does not exist, so the third statement fails after the first two
-- have committed: MySQL commits each DDL statement on its own.
-- +ptah no_transaction
ALTER TABLE deliveries ADD COLUMN status VARCHAR(16);

UPDATE deliveries SET status = 'open';

ALTER TABLE deliveries ADD CONSTRAINT deliveries_status_known CHECK (statuz IN ('open', 'closed'));
