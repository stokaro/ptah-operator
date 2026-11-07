CREATE TABLE t (
    id bigint,
    x bigint,
    CONSTRAINT t_other FOREIGN KEY (id) REFERENCES other.nope (id),
    CONSTRAINT t_nope FOREIGN KEY (x) REFERENCES nope (id)
);
