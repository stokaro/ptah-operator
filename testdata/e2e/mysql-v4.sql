CREATE TABLE e2e_widgets (
  id bigint NOT NULL,
  name varchar(255) NOT NULL,
  note varchar(255),
  enabled boolean,
  PRIMARY KEY (id),
  UNIQUE KEY e2e_widgets_name_unique (name)
);
