CREATE DOMAIN moment AS timestamp;

CREATE TABLE events (
  id bigint PRIMARY KEY,
  created_at moment DEFAULT 'now'
);
