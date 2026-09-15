CREATE TABLE e2e_migration_widgets (
    id INT PRIMARY KEY,
    name VARCHAR(255) NOT NULL
);

INSERT INTO e2e_migration_widgets (id, name) VALUES (1, 'first-edited'), (2, 'second'), (3, 'third');
