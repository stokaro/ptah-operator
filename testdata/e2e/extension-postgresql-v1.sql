CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE e2e_extension_widgets (
  id bigint NOT NULL PRIMARY KEY,
  name text NOT NULL
);
