ALTER TABLE e2e_migration_widgets ADD COLUMN color TEXT;

UPDATE e2e_migration_widgets SET color = 'unset';

ALTER TABLE e2e_migration_widgets ALTER COLUMN color SET NOT NULL;
