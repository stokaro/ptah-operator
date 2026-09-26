-- The same migration with the constraint naming the column it means.
-- +ptah no_transaction
ALTER TABLE deliveries ADD COLUMN status VARCHAR(16);

UPDATE deliveries SET status = 'open';

ALTER TABLE deliveries ADD CONSTRAINT deliveries_status_known CHECK (status IN ('open', 'closed'));
