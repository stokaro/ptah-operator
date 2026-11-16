CREATE TABLE differs_members (
  id bigint NOT NULL PRIMARY KEY,
  who text NOT NULL
);

CREATE VIEW differs_member_ids WITH (security_invoker = true) AS
SELECT id FROM differs_members;

CREATE VIEW differs_member_names WITH (security_invoker = true) AS
SELECT id FROM differs_member_ids;
