CREATE ROLE harness_owner_a LOGIN;
CREATE ROLE harness_grantee_a;

CREATE TABLE differs_owned (
  id integer NOT NULL PRIMARY KEY
);

GRANT SELECT ON differs_owned TO harness_grantee_a;
