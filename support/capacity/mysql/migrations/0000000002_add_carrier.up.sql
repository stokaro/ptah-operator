ALTER TABLE shipments ADD COLUMN carrier TEXT;

UPDATE shipments
   SET carrier = CASE WHEN reference LIKE 'EU-%' THEN 'dhl' ELSE 'ups' END;

ALTER TABLE shipments MODIFY COLUMN carrier TEXT NOT NULL;
