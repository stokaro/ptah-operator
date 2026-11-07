CREATE TABLE t (id bigint, CONSTRAINT t_other FOREIGN KEY (id) REFERENCES other.nope (id));
CREATE INDEX i ON t (missing_column);
