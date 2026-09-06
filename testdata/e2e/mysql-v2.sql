CREATE TABLE e2e_widgets (
  id bigint NOT NULL,
  name varchar(255) NOT NULL,
  note varchar(255),
  PRIMARY KEY (id)
);

CREATE TABLE e2e_order_parent (
  id bigint NOT NULL,
  PRIMARY KEY (id)
);

CREATE TABLE e2e_order_child (
  a bigint NOT NULL,
  b bigint NOT NULL,
  CONSTRAINT b FOREIGN KEY (a) REFERENCES e2e_order_parent (id),
  KEY (b)
);
