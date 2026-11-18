CREATE TABLE differs_notes (
  id bigint NOT NULL PRIMARY KEY,
  owner_name text NOT NULL
);

CREATE VIEW differs_notes_view WITH (security_invoker = true) AS
SELECT id FROM differs_notes;
