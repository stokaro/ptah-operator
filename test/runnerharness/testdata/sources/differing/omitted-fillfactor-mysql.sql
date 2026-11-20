CREATE TABLE differs_omitted (id bigint NOT NULL PRIMARY KEY, name varchar(20));
CREATE INDEX differs_omitted_name ON differs_omitted (name) WITH (fillfactor = 70);
