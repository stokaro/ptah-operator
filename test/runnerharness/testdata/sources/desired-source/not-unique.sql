CREATE TABLE parent (id bigint PRIMARY KEY, code bigint);
CREATE TABLE t (id bigint PRIMARY KEY, code bigint, CONSTRAINT t_code FOREIGN KEY (code) REFERENCES parent (code));
