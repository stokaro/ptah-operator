CREATE TABLE differs_notes (
  id bigint NOT NULL PRIMARY KEY,
  owner_name text NOT NULL
);

CREATE PUBLICATION differs_notes_pub FOR TABLE differs_notes;
