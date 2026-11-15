CREATE TABLE e2e_policy_notes (
  id bigint NOT NULL PRIMARY KEY,
  owner_name text NOT NULL
);

ALTER TABLE e2e_policy_notes ENABLE ROW LEVEL SECURITY;
