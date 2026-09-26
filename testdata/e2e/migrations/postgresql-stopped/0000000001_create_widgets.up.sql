CREATE TABLE e2e_migration_widgets (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL
);

INSERT INTO e2e_migration_widgets (id, name) VALUES (1, 'first'), (2, 'second'), (3, 'third');
