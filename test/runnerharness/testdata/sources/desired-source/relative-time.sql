CREATE TABLE events (
  id bigint PRIMARY KEY,
  created_at timestamptz NOT NULL DEFAULT 'now'
);
