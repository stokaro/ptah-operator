CREATE TABLE kept (id bigint PRIMARY KEY);
CREATE TABLE t (id bigint PRIMARY KEY);
CREATE INDEX i ON t (missing_column);
