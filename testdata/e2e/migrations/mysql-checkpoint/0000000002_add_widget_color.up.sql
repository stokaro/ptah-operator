ALTER TABLE e2e_migration_widgets ADD COLUMN color VARCHAR(255);

UPDATE e2e_migration_widgets SET color = 'unset';

ALTER TABLE e2e_migration_widgets MODIFY color VARCHAR(255) NOT NULL;
