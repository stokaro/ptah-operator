CREATE TABLE differs_notes (
  id bigint NOT NULL PRIMARY KEY,
  owner_name varchar(64) NOT NULL
);

CREATE VIEW differs_notes_view AS
SELECT id, owner_name FROM differs_notes;
