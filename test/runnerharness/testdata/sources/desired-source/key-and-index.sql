CREATE TABLE parent (
    id bigint PRIMARY KEY,
    note text DEFAULT 'declared-default-literal',
    CONSTRAINT parent_note CHECK (note <> 'declared-check-literal')
);
CREATE TABLE t (id bigint, CONSTRAINT t_nope FOREIGN KEY (id) REFERENCES nope (id));
CREATE INDEX i ON t (missing_column);
