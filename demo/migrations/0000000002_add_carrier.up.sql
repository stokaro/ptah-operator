ALTER TABLE shipments ADD COLUMN carrier TEXT;

UPDATE shipments
   SET carrier = CASE WHEN reference LIKE 'EU-%' THEN 'dhl' ELSE 'ups' END;

ALTER TABLE shipments ALTER COLUMN carrier SET NOT NULL;
