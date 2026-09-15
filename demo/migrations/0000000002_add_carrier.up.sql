ALTER TABLE shipments ADD COLUMN carrier TEXT;

UPDATE shipments SET carrier = 'unassigned';

ALTER TABLE shipments ALTER COLUMN carrier SET NOT NULL;
