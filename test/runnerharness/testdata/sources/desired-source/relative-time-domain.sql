CREATE DOMAIN moment AS timestamp DEFAULT 'now';

CREATE TABLE events (
  id bigint PRIMARY KEY,
  created_at moment NOT NULL
);
