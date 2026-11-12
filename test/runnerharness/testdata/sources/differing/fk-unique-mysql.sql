CREATE TABLE differs_parent (
  id bigint NOT NULL PRIMARY KEY
);

CREATE TABLE differs_child (
  id bigint NOT NULL PRIMARY KEY,
  parent_id bigint NOT NULL,
  CONSTRAINT differs_child_parent FOREIGN KEY (parent_id) REFERENCES differs_parent (id),
  CONSTRAINT differs_child_parent_unique UNIQUE (parent_id)
);
