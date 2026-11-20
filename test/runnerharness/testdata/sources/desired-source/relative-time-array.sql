CREATE TABLE events (
  id bigint PRIMARY KEY,
  days date[] NOT NULL DEFAULT '{now}'
);
