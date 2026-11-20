CREATE TABLE events (
  id bigint PRIMARY KEY,
  created_at "pg_catalog"."timestamptz" NOT NULL DEFAULT 'now'
);
